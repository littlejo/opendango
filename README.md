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
- A parent stage without `OUTPUT --as=template` gets an intermediate
  template, kept in the cache (see below).
- If a stage fails, the outputs and the cached templates of the stages
  already finished are kept: the next build resumes from them.
- Stages are built one at a time. `opendango build -j 4` builds up to four
  stages at the same time, each one as soon as its parent is done. After a
  failure, no new stage starts and the running ones finish.

## Cache

Each stage is a layer: when nothing that makes a stage has changed (its
instructions, their variables, the files it copies, the stage it starts
from), the next build reuses its template instead of building it again.
Put the slow, stable steps (`apt-get upgrade`) in a stage of their own.

```sh
opendango build --no-cache               # build everything again
opendango cache prune --older-than=168h  # delete the intermediate cache templates older than a week
opendango cache prune --dry-run          # list them
```

The cache lives in XO, as tagged templates (`opendango-cache:<key>`).
`cache prune` never deletes the `OUTPUT` templates. A stage with a
`BREAKPOINT` is never cached. The base template is identified by the name in
`FROM`: after replacing it under the same name, use `--no-cache`.

## Installing from an ISO

`FROM --iso=<ISO>` boots the VM from an ISO of an ISO SR, with an empty
disk. `SERVE` serves the answer file (preseed, kickstart, autoinstall) and
`BOOT` types the installer command line into the VM console, with the
`boot_command` syntax of Packer:

```dockerfile
FROM --iso=debian-12.11.0-amd64-netinst.iso --template="Debian Bookworm 12" --disk=20G xo:// AS debian
SERVE http
# The template boots with UEFI: GRUB command line.
BOOT <wait10>c<wait2>
BOOT linux /install.amd/vmlinuz auto=true priority=critical url=http://${DANGO_HTTP_ADDR}/preseed.cfg --- quiet<enter>
BOOT initrd /install.amd/initrd.gz<enter>
BOOT boot<enter>
CONNECT ssh --user=debian --timeout=45m
USER root
RUN apt-get install -y nginx
OUTPUT --as=template debian-12-nginx
```

- The connection works as with a cloud image: the installed system needs
  **cloud-init** (it creates the `CONNECT` user with the temporary key from
  XO's cloud-config drive, kept attached during the installation) and the
  **guest tools** (XO only learns the IP address through them: install
  [xen-guest-agent](https://gitlab.com/xen-project/xen-guest-agent) from the
  answer file). Allow for the installation in `CONNECT --timeout`.
- The VM boots from its disk first: the empty disk is skipped until the
  system is installed, so the installer need not eject the ISO.
- Before `OUTPUT`, the ISO is ejected and the cloud-config drive deleted.
- A template without a disk ("Other install media") needs `--disk`; a
  default template ("Debian Bookworm 12") gets its disk resized.

`BOOT` lines are typed in order, once the VM has started and before
`CONNECT`; what to type depends on the boot menu of the ISO (GRUB with UEFI,
isolinux with BIOS: `<esc><wait>auto url=...<enter>` for Debian); quotes and spaces are typed as written, `<wait10>`, `<enter>`,
`<leftCtrlOn>`… are keys, and `\` escapes `$` and `\`. See
[Packer's boot_command reference](https://developer.hashicorp.com/packer/docs/community-tools/boot-command)
for the key names.

## Packer plugins

`FROM plugin://<plugin>-<builder>` runs the builder of a Packer plugin, such
as [vatesfr/packer-plugin-xenserver](https://github.com/vatesfr/packer-plugin-xenserver).
The plugin creates and boots the machine and connects to it; opendango runs
the instructions through that connection; then the plugin produces its
artifact.

```dockerfile
ARG XAPI_PASSWORD
FROM --remote_host=10.0.0.1 --remote_username=root --remote_password=${XAPI_PASSWORD} \
     --clone_template="Debian 13 Cloud-init (Hub)" --vm_name=golden --firmware=bios \
     --network_names='["LAN"]' --cd_files='["cidata/user-data", "cidata/meta-data"]' \
     --ssh_private_key_file=key --format=xva plugin://xenserver-xva
CONNECT ssh --user=debian
RUN apt-get update && apt-get -y upgrade
```

```sh
opendango build --build-arg XAPI_PASSWORD="$XAPI_PASSWORD"
```

- The flags are the plugin's settings, by their Packer names. Lists and maps
  are JSON (`'["LAN"]'`). Paths are relative to the Dangofile directory.
- `CONNECT` sets the plugin's `ssh_username`, `ssh_port` and `ssh_timeout`;
  the credentials (`ssh_password`, `ssh_private_key_file`) are flags. Values of
  flags named `*password*`, `*secret*` or `*token*` are masked in the log.
- Plugins are found in `PACKER_PLUGIN_PATH`, in Packer's plugin directory
  (where `opendango plugin pull` and `packer plugins install` put them), or in
  `PATH`.
- `SERVE <dir>` sets the plugin's `http_directory` (and `--port` its
  `http_port_min` and `http_port_max`), and the `BOOT` lines its
  `boot_command`, with `${DANGO_HTTP_ADDR}` turned into
  `{{ .HTTPIP }}:{{ .HTTPPort }}`: the same Dangofile lines work with
  `plugin://xenserver-iso`. `RUN` gets `$DANGO_HTTP_ADDR` when the plugin
  tells the address of its server.
- `REBOOT` works through the plugin's connection, and `BREAKPOINT` and
  `--on-error=ask` print the `ssh` command of that connection.

### Builders opendango knows

With `plugin://xenorchestra`
([packer-plugin-xenorchestra](https://github.com/littlejo/packer-plugin-xenorchestra)),
a stage behaves like an `xo://` one: `OUTPUT`, `LABEL`, `SERVE --addr`,
multi-stage builds and the cache work the same way. The other plugins set
their artifact with their own flags: `OUTPUT` and `LABEL` are errors, a stage
cannot start from theirs, and they are not cached.

| Instruction | `plugin://xenorchestra` |
|---|---|
| `OUTPUT --as=template <name>` | `template_name` |
| `LABEL` | `tags` (`key=value`) |
| `OUTPUT --format=xva [--compress] <path>` | the plugin's `xenorchestra-xva` post-processor, once per OUTPUT |
| `SERVE --addr` | `http_ip` |
| `FROM <stage>` | `template` = the parent's template |
| cache | the plugin's `xenorchestra-template` data source finds the template tagged with the key |

These settings cannot be FROM flags. Like a native stage, a stage without
`OUTPUT --as=template` deletes the plugin's template once built (or at the
end of the build when other stages start from it). A stage that starts from
another one takes its `endpoint`, `token` and `insecure` flags unless it sets
them, and cannot set `template`, `pool` nor `iso`.

```dockerfile
FROM --template="Debian 13 Cloud-init (Hub)" --network=LAN plugin://xenorchestra AS base
CONNECT ssh --user=debian
RUN apt-get update && apt-get -y upgrade

FROM --cpus=4 base AS app
RUN apt-get install -y nginx
LABEL team=web
OUTPUT --format=xva --compress=zstd nginx.xva
OUTPUT --as=template nginx
```

In a cached plugin stage, `--on-error=ask` does not offer to retry an
instruction (the plugin tags the template for the cache at the end): use
`--no-cache` to fix a build by hand. The plugin serves `SERVE` without
logging the requests.

### Installing plugins

```sh
opendango plugin pull qemu                           # the official HashiCorp plugin
opendango plugin pull github.com/vatesfr/xenserver   # a plugin published on GitHub
opendango plugin pull qemu@1.1.7                     # a given version
opendango plugin search xenserver                    # official plugins, then GitHub repositories
opendango plugin list                                # the plugins plugin:// stages can use
opendango plugin rm qemu@1.1.7                       # remove an installed version
```

- A name alone is the official HashiCorp plugin of that name (from
  releases.hashicorp.com), or else the only GitHub repository named
  `packer-plugin-<name>` that is not a fork. When several match, give the
  source, like in Packer: `github.com/<owner>/<name>` for the repository
  `<owner>/packer-plugin-<name>`.
- The archive is checked against the release's `SHA256SUMS` (signatures are
  not checked), and the plugin must speak the plugin API `x5`. It is installed
  in Packer's plugin directory (`$PACKER_CONFIG_DIR/plugins`, or
  `~/.config/packer/plugins`), in the layout of `packer plugins install`:
  Packer and opendango share their plugins.
- `search` lists the forks that have a star (some maintained plugins are
  forks, such as `vatesfr/xenserver`). Without a token, the GitHub search API
  allows 10 requests a minute: set `GITHUB_TOKEN` to raise the limits.

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
opendango build -j 4            # up to 4 stages (VMs) at the same time
opendango build --no-cache      # ignore the cache of stage templates
opendango fmt -w                # rewrite the Dangofile in its canonical layout
opendango fmt --check           # exit 1 if it is not formatted (CI)
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
3. `RUN`, `COPY`, `ADD`, `ENV`, `USER`, `SHELL`, `WORKDIR`, `REBOOT` and
   `BREAKPOINT` run in order.
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
| `FROM` | `FROM --pool=prod --sr="Local storage" --network="Pool-wide network" --cpus=2 --memory=4G --disk=20G xo://debian-12-cloud AS golden`, `FROM --iso=debian.iso --template="Other install media" --disk=20G xo://` |
| `CONNECT` | `CONNECT ssh --user=debian --port=22 --timeout=10m` |
| `RUN` | `RUN apt-get update`, `RUN ["systemctl", "enable", "nginx"]`, `RUN <<EOF ... EOF` |
| `SHELL` | `SHELL ["/bin/bash", "-o", "pipefail", "-c"]` |
| `COPY` | `COPY --chmod=0644 --chown=www-data site/ /var/www/html/` |
| `ADD` | `ADD --checksum=sha256:<hex> https://example.org/tool.tar.gz /opt/` |
| `ENV` | `ENV DEBIAN_FRONTEND=noninteractive`, `ENV --persist APP_ENV=prod` |
| `USER` | `USER root` (through `sudo`) |
| `WORKDIR` | `WORKDIR /opt/app` |
| `REBOOT` | `REBOOT` |
| `BREAKPOINT` | `BREAKPOINT "check nginx"` |
| `SERVE` | `SERVE --port=8080 --addr=10.0.0.5 http/` (before `CONNECT`) |
| `BOOT` | `BOOT <wait10><esc>auto url=http://${DANGO_HTTP_ADDR}/preseed.cfg<enter>` (before `CONNECT`) |
| `LABEL` | `LABEL org.opencontainers.image.version=1.2 team=web` (with `OUTPUT --as=template`) |
| `OUTPUT` | `OUTPUT --as=template "name"`, `OUTPUT --format=xva --compress=zstd ./out.xva` |

`ENV` values only exist during the build; add `--persist` to write them to
`/etc/environment`. `COPY` sources and `OUTPUT` paths are relative to the
Dangofile directory.

`BREAKPOINT` pauses the build and prints the `ssh` command to log into the
VM with the temporary key; press Enter to resume, Ctrl+C to abort. When
stdin is not a terminal (CI), it is skipped with a message.

`SERVE <dir>` serves a directory of the Dangofile directory over HTTP while
the stage builds (from before the VM starts), for kickstart or preseed files
for instance. `RUN` gets its address as `$DANGO_HTTP_ADDR` (`ip:port`):
`RUN curl -fsS http://$DANGO_HTTP_ADDR/setup.sh | sh`. The IP is the one of
this machine on the route to XO; set `--addr` when the VM reaches it at
another address. The port is a free one unless `--port` is set. The server
listens on every interface and has no authentication: do not serve secrets
on an untrusted network.

`SHELL` changes the shell of the shell form of `RUN` (`/bin/sh -c` by
default), as in Docker: the script is its last argument. The exec form
(`RUN ["..."]`) is not affected.

`LABEL key=value` becomes the XO tag `key=value` on the template of
`OUTPUT --as=template` (required in the stage). A stage inherits the labels
of the stage it starts from; a key set again replaces the value.

`ADD` is `COPY` plus http(s) URL sources, downloaded by opendango (not by
the VM) with mode 0600 unless `--chmod` is set; the file name is the URL's
when the destination is a directory. `--checksum=sha256:<hex>` checks a
single URL source: without it, the stage is not cached (the content is
unknown until downloaded). Unlike Docker, local archives are not extracted:
`ADD` refuses them (use `COPY`, then `RUN tar -xf`), and git sources are not
supported yet.

`CMD`, `ENTRYPOINT`, `EXPOSE`, `HEALTHCHECK`, `VOLUME`,
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
