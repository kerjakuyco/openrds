#!/bin/sh
# sshd for the console, then Docker with its socket open to the docker group.
/usr/sbin/sshd
exec dockerd-entrypoint.sh dockerd --host=unix:///var/run/docker.sock --group docker
