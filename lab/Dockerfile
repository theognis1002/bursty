# QEMU (TCG) runner for the bursty KVM lab.
# Emulates an aarch64 machine with EL2 so the guest kernel gets /dev/kvm.
FROM debian:trixie-slim

RUN apt-get update \
 && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
      qemu-system-arm qemu-efi-aarch64 qemu-utils genisoimage ca-certificates \
 && rm -rf /var/lib/apt/lists/*

COPY entrypoint.sh /usr/local/bin/entrypoint.sh
RUN chmod +x /usr/local/bin/entrypoint.sh

EXPOSE 2222
ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
