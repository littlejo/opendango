# CLAUDE.md

## Project

**opendango** is an open-source replacement for Packer. It builds VM images, but builds are described with **Dockerfile syntax** instead of HCL. The build file is called `Dangofile` (`*.dango` is also accepted).

- Language: **Go**
- License: **Apache-2.0**
- First target builder: **Xen Orchestra** (REST API `/rest/v0`)

## Design principles

- **Keep the Dockerfile grammar unchanged.** Use the BuildKit parser (`github.com/moby/buildkit/frontend/dockerfile/parser`) at the AST level only. Do **not** use `frontend/dockerfile/instructions`, which rejects unknown keywords. Typing the instructions is our job.
- Flags follow the BuildKit form `--key=value`. For standard Dockerfile instructions (`FROM`, `RUN`, `COPY`, …) they come **before** positional arguments, as BuildKit requires. Custom instructions (`CONNECT`, `OUTPUT`, …) accept them anywhere on the line (`CONNECT ssh --user=debian`): BuildKit drops their arguments, so `internal/parser` re-reads the original line.
- Expand variables the Dockerfile way (`${VAR}`), not with Go templates.
- `FROM` carries both the builder and the source, as a URL: `FROM <builder>://<source> [AS <name>]`.
  - Examples: `xo://debian-12-cloud`, `packer://xenserver-iso`.
  - If the name contains spaces, use the UUID or `--template="..."`.
- Secrets come from the environment, never from the Dangofile. The XO variables are the ones of [xo-gocli](https://github.com/littlejo/xo-gocli): `XOA_ENDPOINT`, `XOA_TOKEN`, `XOA_INSECURE`.

## Instructions

| Instruction | Status | Semantics |
|---|---|---|
| `ARG` | MVP | Build variable, overridden with `--build-arg K=V`. Docker scoping: an ARG before the first `FROM` is only visible in `FROM` lines unless the stage declares it again (`ARG NAME`). |
| `FROM` | MVP | `FROM [--flags] <builder>://<source> [AS name]`. Flags configure the builder (xo: `--template`, `--pool`, `--sr`, `--network`, `--cpus`, `--memory`, `--disk`). |
| `FROM <stage>` | done | Multi-stage: starts from the image of an earlier stage (see *Multi-stage builds*). xo flags: `--cpus`, `--memory`, `--disk`, `--sr`, `--network` (not `--template` nor `--pool`). |
| `CONNECT` | MVP (ssh) | `CONNECT ssh --user=<user> [--port=22] [--timeout=10m]`. **Required** before any instruction that needs the VM (`RUN`, `COPY`, `WORKDIR`, `REBOOT`, `BREAKPOINT`, `ENV --persist`). An ephemeral ed25519 key is injected for `<user>` via cloud-init and removed from `authorized_keys` before shutdown. `winrm` is planned. |
| `RUN` | MVP | Shell, exec (JSON) and heredoc forms, run with `/bin/sh -c`. Not expanded at build time: the remote shell expands it, with the ARG and ENV values exported. `--shell=powershell` is planned. |
| `COPY` | MVP | `COPY [--chmod=MODE] [--chown=U[:G]] <src>... <dest>`. Sources are relative to the Dangofile directory and must stay inside it (globs allowed). A directory source copies its contents. The destination is a directory when it ends with `/` or when there are several sources. Files are owned by the current `USER` unless `--chown` is set. |
| `ENV` | MVP | Visible to the following instructions **during the build only**; overrides an ARG of the same name. `ENV --persist` also writes `KEY="value"` to `/etc/environment` (as root). |
| `USER` | MVP | Defaults to the `CONNECT` user. Any other user runs through `sudo -n -H -u <user>`, so the connection user needs passwordless sudo. Groups are not supported. |
| `WORKDIR` | MVP | Created (`mkdir -p`) as the current `USER`, like BuildKit. A relative path is joined to the previous `WORKDIR`; without one, relative paths start from the connection user's home. |
| `REBOOT` | MVP | Runs `reboot` as root, then reconnects until `/proc/sys/kernel/random/boot_id` changes, within the `CONNECT` timeout. |
| `BREAKPOINT` | done | `BREAKPOINT [message]`. Needs `CONNECT`. Writes the ephemeral private key to a temporary file (mode 0600, removed when the build resumes or aborts), prints the `ssh` command for the `CONNECT` user and waits for Enter; Ctrl+C aborts the build (normal cleanup). Skipped with a message when stdin is not a terminal, so a forgotten `BREAKPOINT` does not block a CI build. |
| `OUTPUT` | MVP | Runs after all other instructions, once the VM is stopped: XVA exports first, then the template conversion. `OUTPUT --as=template [name]` (at most one) or `OUTPUT --format=xva [--compress=zstd\|gzip] <path>` (path relative to the Dangofile directory). Without `--as=template`, the VM is destroyed at the end. Other formats are planned. |
| `ADD` | planned | `COPY` plus URL sources. |
| `LABEL` | planned | Metadata or tags attached to the produced image. |
| `BOOT` | planned | Types keystrokes into the VM console, for ISO installs (equivalent of `boot_command`). |
| `SERVE` | planned | Serves a directory over HTTP during the build and exposes `${DANGO_HTTP_ADDR}`. |
| `SHELL` | planned | Changes the shell used by `RUN`. |

### Multi-stage builds

- `FROM <stage>` refers to an earlier stage by its `AS` name (no forward reference, names are unique).
- A stage inherits from its parent: `CONNECT` (it may set its own), `USER`, `WORKDIR` and `ENV`. `ARG` is not inherited, and `FROM` lines only see the global ARGs, like in Docker. Each stage gets its own ephemeral key: cloud-init sees a new instance because XO sets `instance-id` to the VM UUID.
- `opendango build` builds **every** stage, in file order; `--target X` builds X and its ancestors only.
- A stage that a selected stage starts from always ends as a template: its `OUTPUT --as=template`, or a temporary `opendango-tmp-<stage>-<unix time>` template deleted at the end of the build (children first). A stage nobody starts from and without `OUTPUT` triggers a warning: its VM is destroyed.
- On failure the build stops: finished `OUTPUT`s (templates, XVA) of the previous stages are kept; the failing VM and the intermediate templates are deleted, unless `--on-error=keep`.
- Stages run sequentially; parallel builds (`-j`) are planned.

Planned instructions fail with `<X> is not supported yet`. `EXPOSE`, `HEALTHCHECK`, `VOLUME`, `CMD`, `ENTRYPOINT`, `ONBUILD` and `STOPSIGNAL` fail with `<X> has no meaning for a VM image`. Both are reported by `validate`.

## CLI

Implemented (standard library `flag` package; move to cobra only if the CLI grows):

```
opendango build    [-f Dangofile] [--build-arg K=V]... [--target X] [--on-error=abort|keep]
opendango validate [-f Dangofile] [--build-arg K=V]...
opendango version
```

Planned: `--on-error=ask`, `-j N`, `opendango fmt`, `opendango plugins list|install`.

## Architecture

```
cmd/opendango/              CLI (standard library flag)
internal/parser/            BuildKit parser → flat list of commands (recovers custom instruction args)
internal/instructions/      one type per instruction, parsing and validation
internal/plan/              ARG/ENV scoping, variable expansion, stage-level rules → Plan
internal/build/             executor: runs a Plan with a Builder and a Communicator
internal/communicator/ssh/  SSH commands and SFTP uploads
builders/xo/                Xen Orchestra builder
```

Planned: `internal/communicator/winrm`, `internal/output` (qcow2, raw, vmdk, ova, checksums), `builders/packer`.

The `Builder` interface lives in `internal/build`: `Start(cloudConfig)`, `Address`, `Stop(grace)`, `Output`, `Destroy`, `ID`. It grows with the roadmap (`SendKeys` for `BOOT`, `Snapshot` for the cache), not before. Builders are selected by a plain `switch` on the `FROM` scheme (`build.NewBuilder`, `plan.checkBuilder`).

### Dependencies

| Module | Why |
|---|---|
| `github.com/moby/buildkit` (dockerfile `parser` and `shell`) | The Dockerfile grammar and the Docker variable expansion, unchanged. |
| `golang.org/x/crypto/ssh` | SSH client. |
| `github.com/pkg/sftp` | `COPY` uploads. |
| `github.com/coder/websocket` | XO JSON-RPC, only for the conversion to a template. |

The Vates SDK (`xenorchestra-go-sdk`) was evaluated and not used: opendango needs about 8 endpoints, and the SDK covers neither the streamed XVA export nor the conversion to a template, while adding many dependencies.

### Xen Orchestra builder

- It goes through the XO REST API with a thin client (`builders/xo/client.go`). The route reference is served at `/rest/v0/docs` on each instance; the source is `@xen-orchestra/rest-api` in `vatesfr/xen-orchestra`.
- The REST API cannot convert a VM to a template: that single call goes through JSON-RPC (`session.signIn` then `vm.convertToTemplate` on the `/api/` WebSocket, `builders/xo/jsonrpc.go`). Switch to REST when XO adds the endpoint.
- Templates live in the `vm-templates` collection. A default template's id is `<pool>-<uuid>`; `create_vm` takes the bare UUID. Names, ids and UUIDs are all accepted. The same template UUID can exist in several pools (XO Hub templates): `--pool` restricts the lookup, and a template found in several pools without `--pool` is an error.
- Requirements on the template: cloud-init (for the SSH key) and the guest tools (XO only reports the IP address through them).
- Flow:
  1. `POST /pools/{id}/actions/create_vm?sync=true` with the template, CPU, memory, disk, network, cloud-config, `boot: true` and `destroy_cloud_config_vdi: true` (the cloud-config holds the SSH key);
  2. poll `GET /vms/{id}` until `mainIpAddress` (or an `addresses` IPv4) is set;
  3. connect over SSH and run the instructions;
  4. remove the ephemeral key, `shutdown -h now` from the guest, wait for `Halted`, then `clean_shutdown`, then `hard_shutdown`;
  5. export the XVA (`GET /vms/{id}.xva?compress=`) and/or rename (`PATCH`) and convert to a template;
  6. clean up: `DELETE /vms/{id}` unless the VM became the template. Cleanup happens **even on failure or Ctrl+C**, unless `--on-error=keep` is set.
- `vm.convertToTemplate` returns before XO's object cache lists the template: the builder waits for `GET /vm-templates/{id}` before reporting the output done.
- `FROM <stage>` creates the VM from the parent's template (its UUID, in the root's pool). Once converted, `Destroy` deletes the template (`DELETE /vm-templates/{id}`), which is how intermediate templates are removed.

### Packer plugin compatibility

- A plugin is a `packer-plugin-xxx` binary. It is discovered through its `describe` subcommand, then driven over RPC via `packer-plugin-sdk`.
- The plugin's builder creates the VM, then calls the `HookProvision` hook and passes it a connection. **That is when opendango runs the Dangofile's `RUN` and `COPY` instructions.**
- `FROM` flags become the plugin's configuration. `BOOT` and `SERVE` feed `boot_command` and `http_directory`.

## License constraints (mandatory)

- **Never copy code from Packer** (`hashicorp/packer`, under BSL 1.1 since version 1.10). The core must be written from scratch.
- `packer-plugin-sdk` (MPL-2.0) may be imported as a dependency. **Pin its version**: watch for a switch to BSL, and fork from the last MPL release if that happens.
- Third-party Packer plugins run as separate binaries and are never vendored into the code.

## Roadmap

1. **MVP**: parser, `validate`, `xo://` builder, SSH, `ARG`, `ENV`, `USER`, `WORKDIR`, `RUN`, `COPY`, `REBOOT`, `OUTPUT --as=template` and XVA.
2. ~~Multi-stage, `--target`, `BREAKPOINT`~~ (done), parallel builds, `--on-error=ask`, `fmt`.
3. `packer://` compatibility, tested first with `vatesfr/packer-plugin-xenserver`.
4. Native `BOOT` and `SERVE`, then more builders (QEMU, Proxmox, AWS, vSphere).
5. Per-instruction caching based on snapshots.

## Development rules

- Read the existing implementation before proposing changes.
- Prefer simple, explicit implementations over abstractions.
- Do not introduce dependencies without justification.
- Do not implement future roadmap features unless explicitly requested.
- Keep the MVP scope small.
- Never silently change public interfaces or instruction semantics.
- Every new instruction must have parser, validation and execution tests.
- Every builder must have integration tests where applicable.
- Never mock infrastructure behavior in integration tests.
- When a design decision is unclear, explain the alternatives before implementing.
- Update documentation when changing user-visible behavior.

## Conventions

- Run `mise run lint` and `mise run test` before every commit; the CI (`.github/workflows/ci.yml`) runs the same checks plus `-race`. Keep the Go version identical in `mise.toml` and the CI.
- Releases: push a `vX.Y.Z` tag; `.github/workflows/release.yml` runs the CI workflow on the tag, then GoReleaser (`.goreleaser.yaml`), which creates a **draft** GitHub release to review. `mise run release:snapshot` builds the archives locally. The version is injected with `-X main.version`.
- `mise run test:integration` reads `.env` (git-ignored, template in `.env.example`). The integration test cannot run on GitHub-hosted runners (no access to XO): it skips there.
- Write table-driven tests. The parser and the planner are tested without any infrastructure (Dangofiles live in `testdata/`).
- Tests that need a real XO instance use the `integration` build tag and read `XOA_ENDPOINT`, `XOA_TOKEN`, `XOA_INSECURE`, `OPENDANGO_TEST_TEMPLATE`, `OPENDANGO_TEST_USER` (optional: `OPENDANGO_TEST_POOL`, `OPENDANGO_TEST_NETWORK`, `OPENDANGO_TEST_SR`). Run them with `go test -tags integration -timeout 60m ./builders/xo/`.
- Executor unit tests (`internal/build`) use a recording builder and either a scripted communicator or the local `/bin/sh` in a sandbox where `sudo`, `shutdown` and `reboot` are stubs. Never let a unit test run a real `shutdown`, `reboot` or write outside its temporary directories.
- Validation errors point to the file and line: `Dangofile:12: ...`.
