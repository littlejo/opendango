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

> Status: early. One builder (Xen Orchestra).

## Multi-stage builds

A stage can start from the image of an earlier one, like in Docker:

```dockerfile
FROM --memory=2G xo://debian-13-cloud AS golden
CONNECT ssh --user=debian
USER root
RUN apt-get update && apt-get -y upgrade
OUTPUT --as=template golden-debian-13

FROM --cpus=4 golden AS nginx
RUN apt-get install -y nginx
OUTPUT --as=template debian-nginx
```

- A stage inherits `CONNECT`, `USER`, `WORKDIR` and `ENV` from its parent
  (not `ARG`).
- All stages are built; `opendango build --target nginx` builds `nginx` and
  the stages it starts from.
- A parent stage without `OUTPUT --as=template` gets a temporary template,
  deleted at the end of the build.
- If a stage fails, the outputs of the stages already finished are kept.

## Install

Download an archive from the
[releases](https://github.com/littlejo/opendango/releases), or:

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
opendango build --target nginx  # one stage and the stages it starts from
opendango build --on-error=keep # leave the VM in place when the build fails
opendango build --on-error=ask  # on failure: retry the instruction, clean up or keep
```

The variables are the same as [xo-gocli](https://github.com/littlejo/xo-gocli).

## How a build runs

1. The VM is created from the template given in `FROM xo://<template>` and
   booted with a cloud-config that authorizes a temporary SSH key for the
   `CONNECT` user.
2. opendango waits for the IP address reported by the guest tools and
   connects over SSH.
3. `RUN`, `COPY`, `ENV`, `USER`, `WORKDIR`, `REBOOT` and `BREAKPOINT` run in
   order.
4. The temporary key is removed and the VM is shut down.
5. The `OUTPUT` instructions run: XVA exports, then the conversion to a
   template.
6. The VM is deleted unless it became the template, also when the build
   fails or is interrupted (except with `--on-error=keep`, or `k` with
   `--on-error=ask`).

With `--on-error=ask` (from a terminal only), a failed instruction prints the
`ssh` command to inspect the VM and asks: `r` retries the instruction (after
a fix made by hand, for instance), `c` cleans up, `k` keeps the VM and the
intermediate templates. A failure outside the instructions (VM start,
connection, `OUTPUT`) only offers `c` and `k`.

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
| `BREAKPOINT` | `BREAKPOINT "check nginx"` |
| `OUTPUT` | `OUTPUT --as=template "name"`, `OUTPUT --format=xva --compress=zstd ./out.xva` |

`ENV` values only exist during the build; add `--persist` to write them to
`/etc/environment`. `COPY` sources and `OUTPUT` paths are relative to the
Dangofile directory.

`BREAKPOINT` pauses the build and prints the `ssh` command to log into the
VM with the temporary key; press Enter to resume, Ctrl+C to abort. When
stdin is not a terminal (CI), it is skipped with a message.

`ADD`, `LABEL`, `SHELL`, `BOOT` and `SERVE` are planned and
rejected for now. `CMD`, `ENTRYPOINT`, `EXPOSE`, `HEALTHCHECK`, `VOLUME`,
`ONBUILD` and `STOPSIGNAL` have no meaning for a VM image and are rejected.

See [`examples/`](examples/) and [`CLAUDE.md`](CLAUDE.md) for the design and
the roadmap.

## Development

With [mise](https://mise.jdx.dev) (installs the pinned Go version):

```sh
mise run test               # unit tests
mise run lint               # gofmt + go vet
mise run validate-examples  # opendango validate on examples/*/Dangofile
mise run build              # dist/opendango
mise run release:snapshot   # release archives into dist/, nothing published
cp .env.example .env        # then fill it
mise run test:integration   # end-to-end build against a real Xen Orchestra
```

The CI (`.github/workflows/ci.yml`) runs the formatting check, `go vet`, the
unit tests (also with `-race`), the examples validation and the build. The
end-to-end tests need a Xen Orchestra reachable from the runner, so they skip
on GitHub-hosted runners: run them locally with `mise run test:integration`.

To release, push a `vX.Y.Z` tag: the release workflow runs the CI on it, then
GoReleaser creates a draft GitHub release to review and publish.

## License

Apache-2.0
