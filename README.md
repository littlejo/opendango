# opendango

opendango builds VM images, like Packer, but the build is described with the
**Dockerfile syntax** in a `Dangofile`.

```dockerfile
ARG TEMPLATE=debian-12-cloud

FROM --cpus=2 --memory=2G --disk=10G xo://${TEMPLATE} AS nginx
CONNECT ssh --user=debian

USER root
RUN apt-get update && apt-get install -y nginx
COPY --chmod=0644 site/ /var/www/html/
REBOOT
RUN systemctl is-active nginx

OUTPUT --as=template "debian-nginx"
OUTPUT --format=xva --compress=zstd ./dist/debian-nginx.xva
```

> Status: early MVP. One builder (Xen Orchestra), one stage per file.

## Install

```sh
go install github.com/littlejo/opendango/cmd/opendango@latest
```

## Usage

```sh
export XOA_ENDPOINT=https://xo.example.org
export XOA_TOKEN=...            # xo-cli create-token, or xo token create (xo-gocli)
export XOA_INSECURE=true        # only for a self-signed certificate

opendango validate -f Dangofile
opendango build -f Dangofile --build-arg TEMPLATE=debian-13-cloud
opendango build --on-error=keep # leave the VM in place when the build fails
```

The variables are the same as [xo-gocli](https://github.com/littlejo/xo-gocli).

## How a build runs

1. The VM is created from the template given in `FROM xo://<template>` and
   booted with a cloud-config that authorizes a temporary SSH key for the
   `CONNECT` user.
2. opendango waits for the IP address reported by the guest tools and
   connects over SSH.
3. `RUN`, `COPY`, `ENV`, `USER`, `WORKDIR` and `REBOOT` run in order.
4. The temporary key is removed and the VM is shut down.
5. The `OUTPUT` instructions run: XVA exports, then the conversion to a
   template.
6. The VM is deleted unless it became the template, also when the build
   fails or is interrupted (except with `--on-error=keep`).

The template needs **cloud-init** and the **guest tools**.

## Instructions

| Instruction | Example |
|---|---|
| `ARG` | `ARG DEBIAN=12` (before `FROM`: only usable in `FROM`, as in Docker) |
| `FROM` | `FROM --pool=prod --sr="Local storage" --network="Pool-wide network" --cpus=2 --memory=4G --disk=20G xo://debian-12-cloud AS golden` |
| `CONNECT` | `CONNECT ssh --user=debian --port=22 --timeout=10m` |
| `RUN` | `RUN apt-get update`, `RUN ["systemctl", "enable", "nginx"]`, `RUN <<EOF ... EOF` |
| `COPY` | `COPY --chmod=0644 --chown=www-data site/ /var/www/html/` |
| `ENV` | `ENV DEBIAN_FRONTEND=noninteractive`, `ENV --persist APP_ENV=prod` |
| `USER` | `USER root` (through `sudo`) |
| `WORKDIR` | `WORKDIR /opt/app` |
| `REBOOT` | `REBOOT` |
| `OUTPUT` | `OUTPUT --as=template "name"`, `OUTPUT --format=xva --compress=zstd ./out.xva` |

`ENV` values only exist during the build; add `--persist` to write them to
`/etc/environment`. `COPY` sources and `OUTPUT` paths are relative to the
Dangofile directory.

`ADD`, `LABEL`, `SHELL`, `BOOT`, `SERVE` and `BREAKPOINT` are planned and
rejected for now. `CMD`, `ENTRYPOINT`, `EXPOSE`, `HEALTHCHECK`, `VOLUME`,
`ONBUILD` and `STOPSIGNAL` have no meaning for a VM image and are rejected.

See [`examples/`](examples/) and [`CLAUDE.md`](CLAUDE.md) for the design and
the roadmap.

## Development

```sh
go test ./...
# against a real Xen Orchestra:
OPENDANGO_TEST_TEMPLATE=debian-12-cloud OPENDANGO_TEST_USER=debian \
  go test -tags integration -timeout 60m ./builders/xo/
```

## License

Apache-2.0
