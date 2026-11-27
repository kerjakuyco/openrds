# A second server for the end-to-end checks: Docker Engine with SSH, an
# openrds user in the docker group, and util-linux for disk limits.
FROM docker:29-dind
RUN apk add --no-cache openssh-server util-linux e2fsprogs \
 && ssh-keygen -A \
 && adduser -D -s /bin/sh openrds \
 && sed -i 's/^openrds:!/openrds:*/' /etc/shadow \
 && (addgroup -S docker 2>/dev/null || true) && addgroup openrds docker \
 && printf 'PasswordAuthentication no\nPermitRootLogin no\n' >> /etc/ssh/sshd_config \
 && install -d -m 700 -o openrds -g openrds /home/openrds/.ssh
COPY --chmod=0755 entry.sh /entry.sh
ENV DOCKER_TLS_CERTDIR=
ENTRYPOINT ["/entry.sh"]
