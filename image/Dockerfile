# Lambda MicroVM image for one-job GitHub Actions runners.
# Built by Lambda from the zip produced by `just image-artifact`.
# The zip root holds: Dockerfile, supervisor, actions-runner.tar.gz, VERSION.
FROM public.ecr.aws/lambda/microvms:al2023-minimal

ARG ENABLE_DOCKER=true

# Runner dependencies (.NET runtime needs icu/krb5/openssl/zlib) and common CI tools.
# OpenSSL comes from the base image's openssl-snapsafe-libs, which conflicts with openssl-libs.
RUN set -eux; \
    PKG="$(command -v dnf || command -v microdnf)"; \
    "$PKG" install -y --setopt=install_weak_deps=0 \
      shadow-utils tar gzip unzip git jq findutils which hostname procps-ng \
      libicu krb5-libs zlib ca-certificates; \
    if [ "$ENABLE_DOCKER" = "true" ]; then "$PKG" install -y docker iptables-nft; fi; \
    "$PKG" clean all; rm -rf /var/cache/dnf

# Unprivileged runner user. Docker group membership is root-equivalent inside
# the VM; the VM itself is the isolation boundary and lives for one job.
RUN set -eux; \
    useradd --create-home --uid 1001 --shell /bin/bash runner; \
    if getent group docker >/dev/null; then usermod -aG docker runner; fi

# The runner tarball is bundled and its SHA-256 verified at artifact build
# time, so the image build needs no access to github.com.
COPY actions-runner.tar.gz /tmp/actions-runner.tar.gz
RUN set -eux; \
    mkdir -p /home/runner/actions-runner; \
    tar -xzf /tmp/actions-runner.tar.gz -C /home/runner/actions-runner; \
    rm /tmp/actions-runner.tar.gz; \
    chown -R runner:runner /home/runner

COPY --chmod=0755 supervisor /usr/local/bin/supervisor
COPY VERSION /etc/microvm-runner-version

ENV ENABLE_DOCKER=${ENABLE_DOCKER} \
    RUNNER_DIR=/home/runner/actions-runner \
    RUNNER_USER=runner \
    HOOK_PORT=9000

EXPOSE 9000
CMD ["/usr/local/bin/supervisor"]
