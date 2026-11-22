package main

// Encrypted connections. Each installation has its own private certificate
// authority, which signs a server certificate for every database. Clients
// that trust the authority's certificate can verify any database here.

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"maps"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

const (
	caLifetime     = 20 * 365 * 24 * time.Hour
	serverLifetime = 5 * 365 * 24 * time.Hour
	renewBefore    = 90 * 24 * time.Hour // reissued at the next restart once this close to expiry
)

type authority struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pem  []byte
}

func (m *manager) tlsPath(name string) string { return filepath.Join(m.dir, "tls", name) }

func serial() *big.Int {
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		panic(err)
	}
	return n
}

func pemBlock(kind string, der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: kind, Bytes: der})
}

func parseKeyPair(certPEM, keyPEM []byte) (*x509.Certificate, *ecdsa.PrivateKey, error) {
	cb, _ := pem.Decode(certPEM)
	kb, _ := pem.Decode(keyPEM)
	if cb == nil || kb == nil {
		return nil, nil, errors.New("not PEM")
	}
	cert, err := x509.ParseCertificate(cb.Bytes)
	if err != nil {
		return nil, nil, err
	}
	parsed, err := x509.ParsePKCS8PrivateKey(kb.Bytes)
	if err != nil {
		return nil, nil, err
	}
	key, ok := parsed.(*ecdsa.PrivateKey)
	if !ok {
		return nil, nil, errors.New("not an ECDSA key")
	}
	return cert, key, nil
}

func newKey() (*ecdsa.PrivateKey, []byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	return key, pemBlock("PRIVATE KEY", der), err
}

// The installation's certificate authority, created on first use. Its key
// never leaves the control volume.
func (m *manager) authority() (*authority, error) {
	m.tlsMu.Lock()
	defer m.tlsMu.Unlock()
	return m.loadAuthority()
}

func (m *manager) loadAuthority() (*authority, error) {
	if m.ca != nil {
		return m.ca, nil
	}
	certPath, keyPath := m.tlsPath("ca.crt"), m.tlsPath("ca.key")
	certPEM, certErr := os.ReadFile(certPath)
	keyPEM, keyErr := os.ReadFile(keyPath)
	if certErr == nil && keyErr == nil {
		cert, key, err := parseKeyPair(certPEM, keyPEM)
		if err != nil {
			return nil, fmt.Errorf("reading the TLS certificate authority: %w", err)
		}
		m.ca = &authority{cert, key, certPEM}
		return m.ca, nil
	}
	if !errors.Is(certErr, os.ErrNotExist) || !errors.Is(keyErr, os.ErrNotExist) {
		// Never replace half an authority: clients may already trust it.
		return nil, fmt.Errorf("the TLS certificate authority in %s is incomplete", filepath.Dir(certPath))
	}
	key, keyPEM, err := newKey()
	if err != nil {
		return nil, err
	}
	now := time.Now()
	tpl := &x509.Certificate{
		SerialNumber: serial(),
		Subject:      pkix.Name{CommonName: "OpenRDS CA " + randomHex(4), Organization: []string{"OpenRDS"}},
		NotBefore:    now.Add(-time.Hour), NotAfter: now.Add(caLifetime),
		IsCA: true, BasicConstraintsValid: true, MaxPathLenZero: true,
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	cert, _ := x509.ParseCertificate(der)
	certPEM = pemBlock("CERTIFICATE", der)
	if err := writeFile(keyPath, keyPEM); err != nil {
		return nil, err
	}
	if err := writeFile(certPath, certPEM); err != nil {
		return nil, err
	}
	m.ca = &authority{cert, key, certPEM}
	return m.ca, nil
}

// Names a client may use to reach the database: the SSH tunnel or VPS
// loopback, the Compose network and, when set, the public address.
func tlsHosts(r record) []string {
	hosts := []string{"localhost", "127.0.0.1", "::1", "db", "openrds-" + r.Name}
	if r.PublicHost != "" && !slices.Contains(hosts, r.PublicHost) {
		hosts = append(hosts, r.PublicHost)
	}
	return hosts
}

func certHosts(c *x509.Certificate) []string {
	hosts := slices.Clone(c.DNSNames)
	for _, ip := range c.IPAddresses {
		hosts = append(hosts, ip.String())
	}
	return hosts
}

type serverTLS struct {
	cert, key, ca []byte
}

// The database's certificate, reissued when it is missing, signed by another
// authority, close to expiry or missing one of the database's names.
func (m *manager) serverTLS(r record) (serverTLS, error) {
	m.tlsMu.Lock()
	defer m.tlsMu.Unlock()
	ca, err := m.loadAuthority()
	if err != nil {
		return serverTLS{}, err
	}
	certPath, keyPath := m.tlsPath(r.Project+".crt"), m.tlsPath(r.Project+".key")
	hosts := tlsHosts(r)
	certPEM, err1 := os.ReadFile(certPath)
	keyPEM, err2 := os.ReadFile(keyPath)
	if err1 == nil && err2 == nil {
		cert, _, err := parseKeyPair(certPEM, keyPEM)
		if err == nil && cert.CheckSignatureFrom(ca.cert) == nil && time.Until(cert.NotAfter) > renewBefore {
			have, want := certHosts(cert), slices.Clone(hosts)
			slices.Sort(have)
			slices.Sort(want)
			if slices.Equal(have, want) {
				return serverTLS{certPEM, keyPEM, ca.pem}, nil
			}
		}
	}
	key, keyPEM, err := newKey()
	if err != nil {
		return serverTLS{}, err
	}
	now := time.Now()
	tpl := &x509.Certificate{
		SerialNumber: serial(),
		Subject:      pkix.Name{CommonName: "openrds-" + r.Name, Organization: []string{"OpenRDS"}},
		NotBefore:    now.Add(-time.Hour), NotAfter: now.Add(serverLifetime),
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	for _, host := range hosts {
		if ip := net.ParseIP(host); ip != nil {
			tpl.IPAddresses = append(tpl.IPAddresses, ip)
		} else {
			tpl.DNSNames = append(tpl.DNSNames, host)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		return serverTLS{}, err
	}
	certPEM = pemBlock("CERTIFICATE", der)
	if err := writeFile(keyPath, keyPEM); err != nil {
		return serverTLS{}, err
	}
	if err := writeFile(certPath, certPEM); err != nil {
		return serverTLS{}, err
	}
	return serverTLS{certPEM, keyPEM, ca.pem}, nil
}

func (m *manager) forgetTLS(r record) {
	for _, ext := range []string{".crt", ".key"} {
		if err := os.Remove(m.tlsPath(r.Project + ext)); err != nil && !errors.Is(err, os.ErrNotExist) {
			fmt.Fprintf(os.Stderr, "removing TLS files of %s: %v\n", r.Name, err)
		}
	}
}

// PostgreSQL client authentication. Connections from inside the container
// (health checks, dumps) are local; everything else needs a password, and
// TLS when the database requires it.
func pgHBA(r record) string {
	lines := []string{
		"# Managed by OpenRDS. Changes here are replaced at the next restart.",
		"local all all trust",
		"host all all 127.0.0.1/32 scram-sha-256",
		"host all all ::1/128 scram-sha-256",
		"hostssl all all all scram-sha-256",
	}
	if !r.RequireTLS {
		lines = append(lines, "host all all all scram-sha-256")
	}
	return strings.Join(lines, "\n") + "\n"
}

// Files Compose copies into the database container, with the environment
// variables that carry them. Contents never appear in argv or in the
// container's environment.
func (m *manager) injected(r record) (map[string]string, error) {
	t, err := m.serverTLS(r)
	if err != nil {
		return nil, fmt.Errorf("preparing TLS certificates: %w", err)
	}
	files := map[string]string{
		"OPENRDS_TLS_CERT": string(t.cert),
		"OPENRDS_TLS_KEY":  string(t.key),
		"OPENRDS_TLS_CA":   string(t.ca),
	}
	switch r.Engine {
	case "postgres":
		files["OPENRDS_PG_HBA"] = pgHBA(r)
	case "valkey":
		files["OPENRDS_VALKEY_USERS"] = valkeyUsers(r)
	}
	return files, nil
}

// A digest of the injected files. As a container label it makes Compose
// recreate the container when they change, since it does not compare
// secret contents itself.
func digest(files map[string]string) string {
	h := sha256.New()
	for _, k := range slices.Sorted(maps.Keys(files)) {
		fmt.Fprintf(h, "%s=%d:%s\n", k, len(files[k]), files[k])
	}
	return hex.EncodeToString(h.Sum(nil))[:24]
}
