# opendango

opendango builds VM images with the **Dockerfile syntax**: the build is
described in a `Dangofile`, and the machine is created by a **Packer plugin**,
any of the builders Packer users already have (QEMU, Proxmox, vSphere, AWS,
Azure, Xen Orchestra…).

```dockerfile
FROM plugin://qemu AS nginx
USER root
RUN apt-get update && apt-get install -y nginx
COPY --chmod=0644 site/ /var/www/html/
REBOOT
RUN systemctl is-active nginx
```

The machine is described next to it, in `dango.yaml`: the settings of the
plugin, by their Packer names, and how opendango connects to the machine.

```yaml
qemu:
  dango:
    connect: ssh --user=debian
  iso_url: https://cloud.debian.org/images/cloud/trixie/latest/debian-13-genericcloud-amd64.qcow2
  iso_checksum: file:https://cloud.debian.org/images/cloud/trixie/latest/SHA512SUMS
  disk_image: true
  disk_size: 10G
  memory: 2048
  headless: true
  cd_files: [cidata/user-data, cidata/meta-data]
  cd_label: cidata
  ssh_private_key_file: key
  shutdown_command: sudo shutdown -P now
  format: qcow2
  output_directory: dist
  vm_name: nginx.qcow2
```

> Status: early.

## How it works

opendango follows the model of Packer, with the Dangofile instead of HCL and
its instructions instead of provisioners:

1. `FROM plugin://<plugin>-<builder>` starts the builder of a Packer plugin.
   Its settings come from `dango.yaml` and from the `FROM` flags
   (`FROM --memory=4096 plugin://qemu`), which take precedence. The plugin
   creates and boots the machine and connects to it.
2. opendango runs the instructions (`RUN`, `COPY`, `USER`, `REBOOT`…)
   through the plugin's connection.
3. The plugin shuts the machine down and produces its artifact: an image
   file, a template, an AMI…

The plugins are the same binaries as Packer's, installed in the same
directory: Packer and opendango share them. For a few builders, opendango
knows more about the artifact and offers multi-stage builds, `OUTPUT`,
`LABEL` and a cache (see [Builders opendango knows](#builders-opendango-knows)).

## Quick start

Install opendango from the [releases](https://github.com/littlejo/opendango/releases),
or:

```sh
go install github.com/littlejo/opendango/cmd/opendango@latest
```

Then, next to the Dangofile:

```sh
opendango plugin pull qemu    # install the official QEMU plugin
opendango init                # write dango.yaml with every setting of its plugins, commented out
$EDITOR dango.yaml            # uncomment and fill what you need
opendango validate            # the plugin checks the settings, without building
opendango build
```

`opendango plugin describe qemu --search disk` lists the settings, and links
to the plugin's documentation, which tells what they do.

For the example above, `key` is an SSH key pair (`ssh-keygen -t ed25519 -f
key`) and `cidata/` a cloud-init seed: `meta-data` with an `instance-id`,
`user-data` with a cloud-config that authorizes `key.pub` for `debian`. The
plugin attaches it as a CD, and the image is written to `dist/`.

## Instructions

| Instruction | Example |
|---|---|
| `ARG` | `ARG DEBIAN=13` (before `FROM`: only usable in `FROM`, as in Docker), `ARG DANGO_DATE` (see [Builtin variables](#builtin-variables)) |
| `FROM` | `FROM --memory=2048 plugin://qemu AS golden`, `FROM golden AS app` (see [Multi-stage builds](#multi-stage-builds)) |
| `CONNECT` | `CONNECT ssh --user=debian --port=22 --timeout=10m` (or `dango.connect` in [`dango.yaml`](#plugin-settings-file-dangoyaml)) |
| `RUN` | `RUN apt-get update`, `RUN ["systemctl", "enable", "nginx"]`, `RUN <<EOF ... EOF` |
| `SHELL` | `SHELL ["/bin/bash", "-o", "pipefail", "-c"]` |
| `COPY` | `COPY --chmod=0644 --chown=www-data site/ /var/www/html/`, `COPY --from=build /opt/app /usr/local/bin/` (see [Copying from another stage](#copying-from-another-stage)) |
| `ADD` | `ADD --checksum=sha256:<hex> https://example.org/tool.tar.gz /opt/` |
| `ENV` | `ENV DEBIAN_FRONTEND=noninteractive`, `ENV --persist APP_ENV=prod` |
| `USER` | `USER root` (through `sudo`) |
| `WORKDIR` | `WORKDIR /opt/app` |
| `REBOOT` | `REBOOT` |
| `BREAKPOINT` | `BREAKPOINT "check nginx"` |
| `PROVISION` | `PROVISION --playbook_file=site.yml plugin://ansible` (see [Provisioners](#provisioners)) |
| `SERVE` | `SERVE --port=8080 http/` (before `CONNECT`) |
| `BOOT` | `BOOT <wait10><esc>auto url=http://${DANGO_HTTP_ADDR}/preseed.cfg<enter>` (before `CONNECT`) |
| `LABEL` | `LABEL org.opencontainers.image.version=1.2 team=web` (with `OUTPUT --as=template`) |
| `OUTPUT` | `OUTPUT --as=template "name"`, `OUTPUT --format=xva --compress=zstd ./out.xva`, `OUTPUT --format=manifest dist/manifest.json`, `OUTPUT --output=dist/app.box plugin://vagrant` (see [Post-processors](#post-processors)) |

`CONNECT` is required before the instructions that need the machine (`RUN`,
`COPY`, `ADD`, `WORKDIR`, `REBOOT`, `BREAKPOINT`, `PROVISION`, `ENV --persist`), unless
`dango.yaml` gives it (`dango.connect`) or the parent stage has one.

`ENV` values only exist during the build; add `--persist` to write them to
`/etc/environment`. `COPY` sources and `OUTPUT` paths are relative to the
Dangofile directory.

`USER` other than the `CONNECT` user runs through `sudo -n`: the connection
user needs passwordless sudo.

`REBOOT` reboots the machine and waits until it is back (a new boot ID),
within the `CONNECT` timeout.

`BREAKPOINT` pauses the build and prints the `ssh` command to log into the
machine; press Enter to resume, Ctrl+C to abort. When stdin is not a terminal
(CI), it is skipped with a message.

`SERVE <dir>` serves a directory of the Dangofile directory over HTTP while
the stage builds, for kickstart or preseed files for instance. `BOOT` and
`RUN` get its address as `${DANGO_HTTP_ADDR}` (`ip:port`):
`RUN curl -fsS http://$DANGO_HTTP_ADDR/setup.sh | sh`. The port is a free one
unless `--port` is set; `--addr` sets the address the machine reaches it at,
when the builder supports it. The server has no authentication: do not serve
secrets on an untrusted network.

`BOOT` types keys into the machine console once it has started, with the
`boot_command` syntax of Packer: `<wait10>`, `<enter>`, `<leftCtrlOn>`… are
keys, quotes and spaces are typed as written, and `\` escapes `$` and `\`.
See [Packer's boot_command reference](https://developer.hashicorp.com/packer/docs/community-tools/boot-command).

`SHELL` changes the shell of the shell form of `RUN` (`/bin/sh -c` by
default), as in Docker: the script is its last argument. The exec form
(`RUN ["..."]`) is not affected.

`ADD` is `COPY` plus http(s) URL sources, downloaded by opendango (not by
the machine) with mode 0600 unless `--chmod` is set; the file name is the
URL's when the destination is a directory. `--checksum=sha256:<hex>` checks a
single URL source: without it, the stage is not cached (the content is
unknown until downloaded). Unlike Docker, local archives are not extracted:
`ADD` refuses them (use `COPY`, then `RUN tar -xf`), and git sources are not
supported yet.

`OUTPUT --as=template`, `OUTPUT --format=xva` and `LABEL` describe the
artifact, which depends on the builder: they work with the
[builders opendango knows](#builders-opendango-knows); the other plugins set
their artifact with their own flags (`--output_directory`, `--ami_name`…).
The [post-processors](#post-processors) work with every builder.

`CMD`, `ENTRYPOINT`, `EXPOSE`, `HEALTHCHECK`, `VOLUME`, `ONBUILD` and
`STOPSIGNAL` have no meaning for a VM image and are rejected.

### Builtin variables

opendango computes variables for each build, like the predefined ARGs of
Docker:

| Variable | Value |
|---|---|
| `DANGO_TIMESTAMP` | the start of the build, in Unix seconds (`1794000000`) |
| `DANGO_DATE` | its date, UTC (`20261106`) |
| `DANGO_TIME` | its time, UTC (`214510`) |
| `DANGO_BUILD_ID` | a UUID, also the run UUID of the [manifest](#post-processors) |
| `DANGO_GIT_COMMIT` | the short commit of the repository of the Dangofile (empty outside of git) |

```dockerfile
FROM --vm_name=golden-${DANGO_TIMESTAMP} plugin://xenorchestra AS golden
ARG DANGO_DATE
RUN echo "built on $DANGO_DATE" > /etc/build-date
OUTPUT --as=template golden-${DANGO_DATE}
```

- They are the same in every stage of a build.
- `FROM` lines and [`dango.yaml`](#plugin-settings-file-dangoyaml) see them;
  a stage sees them once it declares them (`ARG DANGO_DATE`), like a global
  ARG.
- `--build-arg DANGO_DATE=…` replaces one (to rebuild an image with the same
  name, for instance).
- The time and the build id do not change the [cache](#cache) key: a name
  with a timestamp does not build the image again, and a stage found in the
  cache keeps the name it was built with. The git commit does change it.
- The shell patterns of Docker apply: `${DANGO_GIT_COMMIT:-dev}`.

## Plugins

`FROM [--flags] plugin://<plugin>-<builder>` runs a builder of a Packer
plugin. The source is the Packer source type: `qemu`, `proxmox-iso`,
`amazon-ebs`, `xenserver-iso`…

- The flags are the builder's settings, by their Packer names. Lists and maps
  are JSON (`--network_names='["LAN"]'`). Paths are relative to the Dangofile
  directory.
- They can also go to a `dango.yaml` file next to the Dangofile, which
  the `FROM` flags override (see [Plugin settings file](#plugin-settings-file-dangoyaml)).
- `opendango plugin describe <source>` lists them, with their types
  (`--search disk` filters them) and a link to the plugin's documentation,
  which tells what they do. `opendango validate` checks them with the
  plugin: a misspelled flag gets the closest setting
  (`plugin://qemu has no setting "disk_sise", did you mean disk_size?`),
  and the plugin reports what is missing or invalid, without creating
  anything (`--offline` skips this).
- `CONNECT` (or `dango.connect`) sets `communicator`, `ssh_username`,
  `ssh_port` and `ssh_timeout`.
  The credentials (`ssh_password`, `ssh_private_key_file`…) are flags, or the
  plugin's own temporary key when it makes one.
- Secrets come from the environment: pass them with `ARG` and
  `--build-arg NAME=value`. Values of flags named `*password*`, `*secret*` or
  `*token*` are masked in the log.
- `SERVE <dir>` sets the plugin's `http_directory` (and `--port` its
  `http_port_min` and `http_port_max`): the plugin serves the directory. The
  `BOOT` lines become its `boot_command`, with `${DANGO_HTTP_ADDR}` turned
  into `{{ .HTTPIP }}:{{ .HTTPPort }}`, and `RUN` gets `$DANGO_HTTP_ADDR` when
  the plugin tells the address of its server.
- `REBOOT`, `BREAKPOINT` and `--on-error=ask` work through the plugin's
  connection.
- `--on-error` becomes the plugin's `on_error` (`abort` → clean up,
  `keep` → keep the machine, `ask` → ask).

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

### Plugin settings file (`dango.yaml`)

`dango.yaml`, next to the Dangofile (or `--config=<file>`), holds the
settings by source type, so that the Dangofile keeps the build steps:

```yaml
qemu:
  dango:                       # opendango's options, not the plugin's
    connect: ssh --user=debian --timeout=15m
  iso_url: https://cloud.debian.org/images/cloud/${DEBIAN}/latest/debian-13-genericcloud-amd64.qcow2
  disk_size: 10G
  cd_files: [cidata/user-data, cidata/meta-data]
```

```dockerfile
ARG DEBIAN=trixie

FROM --memory=4096 --vm_name=nginx.qcow2 plugin://qemu AS nginx
RUN apt-get update
```

- Every stage of that source gets the section. Its `FROM` flags take
  precedence, and a `CONNECT` in the stage replaces `dango.connect`.
- `dango` holds opendango's options:
  - `connect`: the `CONNECT` of the stages without one, with the syntax of
    the instruction. `SERVE` and `BOOT` may still come first in the stage.
  - `parent`: the settings of a stage that starts from another one
    (`FROM <stage>`), over the section: how the plugin starts from the
    parent's artifact (see [Multi-stage builds](#multi-stage-builds)).
- A stage that starts from another one inherits its parent's connection,
  and leaves out the settings of the source (`template`, `pool`, `iso` for
  xenorchestra).
- `${VAR}` is replaced with the global ARGs, like in `FROM`. Lists and maps
  are YAML.
- No secrets: settings named `*password*`, `*secret*` or `*token*` are
  refused (use `ARG` and `--build-arg` in the `FROM` line).
- The settings are part of the cache key.

`opendango init` writes a `dango.yaml` with every setting of the plugins of
the Dangofile (or of the sources given: `opendango init qemu
xenorchestra`), commented out with its type: uncomment what you need, a
setting left out keeps the plugin's default. `--force` overwrites an
existing file.

```yaml
qemu:
  # dango:                    # opendango's options, not the plugin's
  #   connect: ssh --user=debian --timeout=10m   # CONNECT of the stages without one
  # What the settings do: https://developer.hashicorp.com/packer/integrations/hashicorp/qemu
  # accelerator: ""                      # string
  # cd_files: []                         # list of string
  # cpus: 0                              # number
  # disk_image: false                    # bool
  ...
```

### Managing plugins

```sh
opendango plugin pull qemu                           # an official plugin
opendango plugin pull github.com/vatesfr/xenserver   # a plugin published on GitHub
opendango plugin pull qemu@1.1.7                     # a given version
opendango plugin search xenserver                    # official plugins, then GitHub repositories
opendango plugin list                                # the plugins plugin:// stages can use
opendango plugin describe qemu --search disk         # the settings (FROM flags) of a builder
opendango plugin rm qemu@1.1.7                       # remove an installed version
```

- A name alone is the official plugin of that name (releases.hashicorp.com),
  or else the only GitHub repository named `packer-plugin-<name>` that is not
  a fork. When several match, give the source, like in Packer:
  `github.com/<owner>/<name>` for the repository `<owner>/packer-plugin-<name>`.
- The archive is checked against the release's `SHA256SUMS` (signatures are
  not checked), and the plugin must speak the plugin API `x5`.
- Plugins are installed in Packer's plugin directory
  (`$PACKER_CONFIG_DIR/plugins`, or `~/.config/packer/plugins`), in the
  layout of `packer plugins install`. They are found there, in
  `PACKER_PLUGIN_PATH` and in `PATH`.
- `search` lists the forks that have a star (some maintained plugins are
  forks). Without a token, the GitHub search API allows 10 requests a minute:
  set `GITHUB_TOKEN` to raise the limits.

### Builders opendango knows

A plugin sets its artifact with its own settings, which opendango does not
interpret. For the builders of this table, opendango knows the artifact (a
template), so their stages get everything a stage can do: `OUTPUT`, `LABEL`,
`SERVE --addr`, multi-stage builds and the cache.

| | `plugin://xenorchestra` ([packer-plugin-xenorchestra](https://github.com/littlejo/packer-plugin-xenorchestra)) |
|---|---|
| `OUTPUT --as=template <name>` | `template_name` |
| `LABEL` | `tags` (`key=value`) |
| `OUTPUT --format=xva [--compress] <path>` | the plugin's `xenorchestra-xva` post-processor, once per `OUTPUT` |
| `SERVE --addr` | `http_ip` |
| `FROM <stage>` | `template` = the parent's template; `endpoint`, `token` and `insecure` are inherited |
| cache | the plugin's `xenorchestra-template` data source finds the template tagged with the key |

These settings cannot be FROM flags. A stage without `OUTPUT --as=template`
deletes the plugin's template once built (an XVA stays), or at the end of the
build when other stages start from it.

The other plugins: `OUTPUT` and `LABEL` are errors and they are not cached;
a stage can start from theirs with `dango.parent` (see
[Multi-stage builds](#multi-stage-builds)).

## Multi-stage builds

A stage can start from the image of an earlier one, like in Docker:

```dockerfile
FROM plugin://qemu AS base
USER root
RUN apt-get update && apt-get -y upgrade

FROM --vm_name=nginx.qcow2 --output_directory=dist/nginx base AS nginx
RUN apt-get install -y nginx
```

The child stage builds with the plugin of its parent, from the parent's
artifact. How the plugin starts from an artifact depends on the plugin: tell
it in `dango.parent`, with `${DANGO_PARENT_ID}` (the id of the parent's
artifact) and `${DANGO_PARENT_FILE}` (its first file):

```yaml
qemu:
  dango:
    connect: ssh --user=debian
    parent:                           # over the section, for FROM <stage>
      iso_url: ${DANGO_PARENT_FILE}   # the parent's image
      iso_checksum: none
  iso_url: https://cloud.debian.org/images/cloud/trixie/latest/debian-13-genericcloud-amd64.qcow2
  iso_checksum: file:https://cloud.debian.org/images/cloud/trixie/latest/SHA512SUMS
  disk_image: true
  output_directory: dist/base
  vm_name: base.qcow2

amazon-ebs:
  dango:
    parent:
      source_ami: ${DANGO_PARENT_ID#*:}   # eu-west-3:ami-0abc… → ami-0abc…
```

- The values are expanded when the child starts, with the global ARGs and
  the parent's artifact: the shell patterns of Docker work
  (`${DANGO_PARENT_ID#*:}`).
- The [builders opendango knows](#builders-opendango-knows) need no
  `dango.parent` (xenorchestra starts from the parent's template); with
  one, it replaces their own way.
- With another plugin, the parent's artifact is the plugin's own (a file, an
  AMI): it is kept. With a known builder, a parent without
  `OUTPUT --as=template` gets an intermediate template, deleted at the end of
  the build or kept in the cache (see below).
- A stage inherits `CONNECT`, `USER`, `SHELL`, `WORKDIR`, `ENV` and `LABEL`
  from its parent (not `ARG`).
- All stages are built; `opendango build --target nginx` builds `nginx` and
  the stages it starts from.
- If a stage fails, the outputs and the cached templates of the stages
  already finished are kept: the next build resumes from them.
- Stages are built one at a time. `opendango build -j 4` builds up to four
  stages at the same time, each one as soon as its parent is done. After a
  failure, no new stage starts and the running ones finish. Independent
  stages may use different plugins.

## Copying from another stage

`COPY --from=<stage>` copies files out of an earlier stage, which can have
nothing to do with this one (another system, another plugin), like a Docker
build stage and its runtime image:

```dockerfile
# Ubuntu, with the compilers.
FROM --source_ami=ami-0ubuntu0000000000 --ami_name=build plugin://amazon-ebs AS build
CONNECT ssh --user=ubuntu
USER root
RUN apt-get install -y golang && go build -o /opt/app ./...

# CentOS, the image to keep.
FROM --source_ami=ami-0centos0000000000 --ami_name=app plugin://amazon-ebs AS app
CONNECT ssh --user=ec2-user
USER root
COPY --from=build --chmod=0755 /opt/app /usr/local/bin/app
RUN systemctl enable app
```

- The paths are those of the source stage's machine, from its root
  (no patterns). A directory copies its contents, like `COPY`; `--chmod` and
  `--chown` work the same way, and symbolic links stay links.
- At the end of the source stage, while its machine runs, opendango fetches
  the paths as root (`tar`, which the machine needs) to this machine; the
  stage that copies them waits for it. `--target app` builds `build` too.
- A stage that only feeds `COPY --from` needs no `OUTPUT`: its machine is
  deleted once the files are fetched.
- The fetched files are kept in a cache (`~/.cache/opendango/files`): when
  `build` has not changed, the next build takes them from there and starts
  no machine for it. When `build` is in the cache as a template but its
  files are not (a `COPY --from` added later), a machine starts from the
  template only to fetch them.

## Provisioners

`PROVISION [--settings] plugin://<provisioner>` runs the provisioner of a
Packer plugin among the instructions: Ansible, Chef, Puppet, Salt…

```dockerfile
FROM plugin://qemu AS web
RUN apt-get update
PROVISION --playbook_file=site.yml --extra_arguments='["--tags", "web"]' plugin://ansible
RUN systemctl is-active nginx
```

```sh
opendango plugin pull ansible    # ansible (remote) and ansible-local
```

- The settings are the provisioner's, by their Packer names (lists and maps
  in JSON), over its section of [`dango.yaml`](#plugin-settings-file-dangoyaml)
  (`ansible:`).
- The provisioner gets the stage's connection, like with Packer: the
  plugin's in a `plugin://` stage, opendango's own with `xo://`, and its
  data (host, port, user, key). `plugin://ansible` runs `ansible-playbook`
  on this machine, which must have it; `plugin://ansible-local` runs it in
  the machine.
- It runs as the connection's user: `USER`, `ENV` and `WORKDIR` do not apply
  (with Ansible, use `become`).
- The files and directories of the Dangofile directory that its settings
  name (`site.yml`, `roles/`) are part of the cache key: changing the
  playbook builds the stage again.
- `opendango validate` has the provisioner check its settings; with
  `--on-error=ask`, a failed `PROVISION` can be retried.

## Post-processors

Once a stage has made its artifact, `OUTPUT` can process it, in the order of
the Dangofile, like the post-processors of Packer:

```dockerfile
FROM plugin://qemu AS golden
RUN apt-get update && apt-get -y upgrade
OUTPUT --format=checksum dist/SHA256SUMS
OUTPUT --format=compress dist/golden.tar.gz
OUTPUT --format=manifest dist/manifest.json
OUTPUT --output=dist/golden.box plugin://vagrant
```

| Output | What it does |
|---|---|
| `--format=manifest <path>` | Adds the build to a JSON manifest, in the format of Packer's: `builds` (`name`, `builder_type`, `build_time`, `files` with their size, `artifact_id`, `packer_run_uuid`) and `last_run_uuid`. The builds of the previous runs stay; several stages can write to the same file. |
| `--format=checksum [--type=sha256] <path>` | Writes the checksums of the artifact's files, in the format of `sha256sum` (`sha256sum -c` checks them). Types: `md5`, `sha1`, `sha224`, `sha256` (default), `sha384`, `sha512`. |
| `--format=compress <path>` | Puts the artifact's files in an archive: `.tar`, `.tar.gz` (or `.tgz`), `.zip`, by the extension. The files stay. |
| `[--settings] plugin://<post-processor>` | Runs the post-processor of a plugin (`plugin://vagrant`, `plugin://docker-import`…) on the artifact (its id, files and builder). Its settings are the flags, by their Packer names, over its section of [`dango.yaml`](#plugin-settings-file-dangoyaml). |

- The artifact's files are those of the plugin (an image, a box) and the XVA
  exports of the stage. A template or an AMI has none: `checksum` and
  `compress` need files.
- A stage found in the cache runs its post-processors on the cached template.
- The post-processors are not part of the cache key: adding one does not
  build the stage again.
- `opendango validate` has the plugins' post-processors check their
  settings.
- `manifest`, `checksum` and `compress` are written by opendango (Packer's
  are part of Packer); the plugins' post-processors are the same as with
  Packer. They process the artifact of the build: they are not chained.

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

The cached templates are tagged `opendango-cache:<key>`; the files of
`COPY --from` are kept in `~/.cache/opendango/files/<key>`, which can be
deleted at any time. `cache prune` works
on Xen Orchestra (`XOA_*` variables) and never deletes the `OUTPUT`
templates. Not cached: the stages of plugins opendango
does not know, a stage with a `BREAKPOINT`, a stage with an `ADD` of a URL
without `--checksum` (and their children). The base template is identified by
the name in `FROM`: after replacing it under the same name, use `--no-cache`.
In a cached plugin stage, `--on-error=ask` does not offer to retry an
instruction: use `--no-cache` to fix a build by hand.

## Installing from an ISO

`SERVE` serves the answer file (preseed, kickstart, autoinstall) and `BOOT`
types the installer command line into the console:

```dockerfile
FROM --template="Debian Bookworm 12" --iso=debian-12.11.0-amd64-netinst.iso --disk_size=20G \
     --ip_wait_timeout=45m plugin://xenorchestra AS debian
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

What to type depends on the boot menu of the ISO (GRUB with UEFI, isolinux
with BIOS: `<esc><wait>auto url=...<enter>` for Debian). Allow for the
installation in `CONNECT --timeout`. The installed system must accept the
connection: with xenorchestra, it needs cloud-init (the temporary key comes
from the cloud-config drive) and the guest tools (the IP address).

## Commands

```sh
opendango init [qemu ...]       # write a dango.yaml with the settings of the plugins
opendango validate -f Dangofile --build-arg K=V   # check without building (plugins check their flags)
opendango validate --offline    # without asking the plugins
opendango build -f Dangofile --build-arg K=V
opendango build --target nginx  # one stage and the stages it starts from
opendango build -j 4            # up to 4 stages at the same time
opendango build --no-cache      # ignore the cache
opendango build --on-error=keep # leave the machine in place when the build fails
opendango build --on-error=ask  # on failure: retry the instruction, clean up or keep
opendango fmt -w                # rewrite the Dangofile in its canonical layout
opendango fmt --check           # exit 1 if it is not formatted (CI)
opendango plugin pull|search|list|describe|rm
opendango cache prune [--older-than=DURATION] [--dry-run]
```

With `--on-error=ask` (from a terminal only), a failed instruction prints the
`ssh` command to inspect the machine and asks: `r` retries the instruction
(after a fix made by hand, for instance), `c` cleans up, `k` keeps the
machine and the intermediate templates. A failure outside the instructions
(start, connection, `OUTPUT`) only offers `c` and `k`.

## Native builder: Xen Orchestra (`xo://`)

opendango also has a builder of its own for [Xen Orchestra](https://xen-orchestra.com),
written before the plugin support: `FROM xo://<template>` behaves like
`plugin://xenorchestra`, without a plugin to install.

```dockerfile
FROM --pool=prod --sr="Local storage" --network="Pool-wide network" --cpus=2 --memory=4G --disk=20G \
     xo://debian-13-cloud AS golden
CONNECT ssh --user=debian
USER root
RUN apt-get update && apt-get -y upgrade
OUTPUT --as=template golden-debian-13
OUTPUT --format=xva --compress=zstd ./dist/golden.xva
```

```sh
export XOA_ENDPOINT=https://xo.example.org
export XOA_TOKEN=...            # xo-cli create-token, or xo token create (xo-gocli)
export XOA_INSECURE=true        # only for a self-signed certificate
opendango build
```

The variables are the same as [xo-gocli](https://github.com/littlejo/xo-gocli)
(and the plugin's).

- **FROM flags**: `--template` (or `xo://<template>`), `--pool`, `--sr`,
  `--network`, `--cpus`, `--memory`, `--disk`, `--iso`. A stage that starts
  from another one accepts `--cpus`, `--memory`, `--disk`, `--sr` and
  `--network`.
- **A build**: the VM is created from the template with a cloud-config that
  authorizes a temporary SSH key for the `CONNECT` user; opendango waits for
  the IP address reported by the guest tools and connects; the instructions
  run; the key is removed and the VM shut down; the XVA exports, then the
  conversion to a template; the VM is deleted unless it became the template
  (also when the build fails, except with `--on-error=keep`). The template
  needs **cloud-init** and the **guest tools**.
- **`--iso=<ISO>`** boots from an ISO of an ISO SR, with the cloud-config
  drive kept during the installation and the ISO ejected before `OUTPUT`.
  The VM boots from its disk first, so the installer need not eject the ISO.
  A template without a disk ("Other install media") needs `--disk`.
- **`SERVE`** advertises the address of this machine on the route to XO,
  unless `--addr` is set.
- **`LABEL`** and the cache use XO tags on the templates.

## Development

With [mise](https://mise.jdx.dev) (installs the pinned Go version):

```sh
mise run test               # unit tests
mise run lint               # gofmt + go vet
mise run validate-examples  # opendango validate on examples/*/Dangofile
mise run build              # dist/opendango
mise run release:snapshot   # release archives into dist/, nothing published
cp .env.example .env        # then fill it
mise run test:integration   # end-to-end builds against real infrastructure
```

The unit tests drive a real test plugin
(`builders/packer/testdata/packer-plugin-opendangotest`) through the plugin
protocol, without any infrastructure. The CI (`.github/workflows/ci.yml`)
runs the formatting check, `go vet`, the unit tests (also with `-race`), the
examples validation and the build. The end-to-end tests need the
infrastructure of the builders (a Xen Orchestra reachable from the runner),
so they skip on GitHub-hosted runners: run them locally with
`mise run test:integration`.

To release, push a `vX.Y.Z` tag: the release workflow runs the CI on it, then
GoReleaser creates a draft GitHub release to review and publish.

See [`examples/`](examples/) and [`CLAUDE.md`](CLAUDE.md) for the design and
the roadmap.

## License

Apache-2.0
