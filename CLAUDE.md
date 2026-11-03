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
- A stage that a selected stage starts from always ends as a template: its `OUTPUT --as=template`, or an intermediate template. With the cache (default), the intermediate template is `opendango-cache-<stage>-<unix time>` and stays (see *Cache*); with `--no-cache` (or when the stage is not cacheable), it is a temporary `opendango-tmp-<stage>-<unix time>` deleted at the end of the build (children first). A stage nobody starts from and without `OUTPUT` triggers a warning: its VM is destroyed.
- On failure the build stops: finished `OUTPUT`s (templates, XVA) and cached templates of the previous stages are kept, so the next build resumes from them; the failing VM and the temporary templates are deleted, unless `--on-error=keep` (or the answer `k` to `--on-error=ask`).
- Stages run one at a time by default (`-j 1`). With `-j N`, up to N stages run at the same time; a stage starts once its parent is done, in file order. After a failure, no new stage starts and the running ones finish (their `OUTPUT`s are kept); Ctrl+C stops everything. All failures are reported. Log lines are prefixed with the stage name, and `BREAKPOINT` / `--on-error=ask` questions are asked one at a time.

### Cache

The cache works per stage: the stages are the layers. It is on by default; `opendango build --no-cache` neither reads nor fills it.

- **Key** (`internal/build/cache.go`): a sha256 of the parent's key, the expanded `FROM` (builder, source or stage, flags), `CONNECT` (protocol, user, port), every step with its expanded values (`ARG`, `ENV`, `USER`, `WORKDIR`, `RUN` script, `COPY` arguments plus the path, mode and content of every source file) and the `OUTPUT --as=template` name. Line numbers and layout are left out. `cacheVersion` is bumped when the key changes meaning.
- **Not cached**: a stage with a `BREAKPOINT`, and its descendants (manual changes are not reproducible); a stage in which `--on-error=ask` retried an instruction; a stage that does not end as a template.
- **Limit**: the base template is keyed by the name written in `FROM`; a template replaced under the same name is not detected (like a Docker tag). Use `--no-cache`, or the template UUID.
- **Storage** (XO tags): every cached template has `opendango-cache:<key>`; intermediate ones also have `opendango-cache-created:<unix time>`. The lookup is in the pool of the template the VM would start from. On a hit, the stage starts no VM: its children start from the cached template, its `XVA` outputs are exported from it (`GET /vm-templates/{id}.xva`), and the final `USER`, `WORKDIR`, `ENV` are replayed from the instructions.
- **Cleanup**: `opendango cache prune [--older-than=DURATION] [--dry-run]` deletes the intermediate cache templates (with the creation tag) only, never the `OUTPUT` templates. Nothing is deleted automatically.
- Per-instruction caching (snapshots) is not done: there is no documented way to create a VM from a snapshot in the REST API, snapshots belong to the build VM, and VHD chains are limited to about 30 snapshots.

Planned instructions fail with `<X> is not supported yet`. `EXPOSE`, `HEALTHCHECK`, `VOLUME`, `CMD`, `ENTRYPOINT`, `ONBUILD` and `STOPSIGNAL` fail with `<X> has no meaning for a VM image`. Both are reported by `validate`.

## CLI

Implemented (standard library `flag` package; move to cobra only if the CLI grows):

```
opendango build    [-f Dangofile] [--build-arg K=V]... [--target X] [-j N] [--no-cache] [--on-error=abort|keep|ask]
opendango validate [-f Dangofile] [--build-arg K=V]...
opendango fmt      [-f Dangofile] [-w | --check]
opendango cache prune [--older-than=DURATION] [--dry-run]
opendango version
```

`--on-error=ask` needs a terminal on stdin (checked with `golang.org/x/term`; `opendango build` exits with code 2 otherwise). When an instruction fails, it prints the `ssh` command (if the VM was reached and the key is still installed) and asks `[r]` retry the instruction (after a new connection), `[c]` clean up (like `abort`) or `[k]` keep (like `keep`). Failures outside the instructions (start, connection, stop, `OUTPUT`, a later stage's builder) offer only `c` and `k`. End of input and Ctrl+C mean `c`. The returned error is the instruction's, whatever the answer.

`opendango fmt` (`internal/format`) prints the canonical layout, like gofmt; `-w` rewrites the file, `--check` exits with code 1 when it differs (the CI runs it on `examples/`). It only changes the layout: upper-case keywords with one space before the arguments, no trailing spaces, at most one blank line between instructions, one blank line before each `FROM` but the first (above its comments), a final newline. Arguments, continuation lines and heredoc bodies are never changed; the tests check that the formatted file parses to the same instructions and that formatting is stable.

Planned: `opendango plugins list|install`.

## Architecture

```
cmd/opendango/              CLI (standard library flag)
internal/parser/            BuildKit parser → flat list of commands (recovers custom instruction args)
internal/instructions/      one type per instruction, parsing and validation
internal/plan/              ARG/ENV scoping, variable expansion, stage-level rules → Plan
internal/format/            opendango fmt: canonical layout of a Dangofile
internal/build/             executor: runs a Plan with a Builder and a Communicator
internal/communicator/ssh/  SSH commands and SFTP uploads
builders/xo/                Xen Orchestra builder
builders/packer/            Packer plugins: discovery, protocol, communicator adapter
```

Planned: `internal/communicator/winrm`, `internal/output` (qcow2, raw, vmdk, ova, checksums).

The `Builder` interface lives in `internal/build`: `Start(cloudConfig)`, `Address`, `Stop(grace)`, `Output`, `Destroy`, `ID`, `FindCache(key)`, `Tag(tag)`. It grows with the roadmap (`SendKeys` for `BOOT`), not before. Builders are selected by a plain `switch` on the `FROM` scheme (`build.NewBuilder`, `plan.checkBuilder`).

### Dependencies

| Module | Why |
|---|---|
| `github.com/moby/buildkit` (dockerfile `parser` and `shell`) | The Dockerfile grammar and the Docker variable expansion, unchanged. |
| `golang.org/x/crypto/ssh` | SSH client. |
| `github.com/pkg/sftp` | `COPY` uploads. |
| `github.com/coder/websocket` | XO JSON-RPC, only for the conversion to a template. |
| `github.com/hashicorp/packer-plugin-sdk` v0.6.13 (pinned, MPL-2.0) | The client side of the Packer plugin protocol (`rpc`) for packer://. Requires the `go-cty` replace below. |
| `github.com/nywilken/go-cty` v1.13.3 (MIT, `replace` of `zclconf/go-cty`) | go-cty with gob encoding, required by the SDK's `rpc` package. |
| `golang.org/x/term` | Detects a terminal on stdin for `--on-error=ask` and `BREAKPOINT` (`/dev/null` is a character device too, so `os.ModeCharDevice` is not enough). Already required by `x/crypto`. |

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
  6. clean up: `DELETE /vms/{id}` unless the VM became the template. Cleanup happens **even on failure or Ctrl+C**, unless `--on-error=keep` is set or `k` is answered to `--on-error=ask`.
- `vm.convertToTemplate` returns before XO's object cache lists the template: the builder waits for `GET /vm-templates/{id}` before reporting the output done.
- `FROM <stage>` creates the VM from the parent's template (its UUID, in the root's pool). Once converted, `Destroy` deletes the template (`DELETE /vm-templates/{id}`), which is how temporary templates are removed. A builder that found its template in the cache (`FindCache`) never deletes it. Tags are added with `PUT /vm-templates/{id}/tags/{tag}`.

### Packer plugin compatibility

`FROM [--flags] packer://<plugin>-<builder> [AS name]` (`builders/packer`, `internal/build/packer.go`). Only builders are supported (no provisioners, post-processors or datasources).

- **Discovery**: `packer-plugin-<name>` binaries in `PACKER_PLUGIN_PATH`, then Packer's plugin directory (`$PACKER_CONFIG_DIR` or `~/.config/packer/plugins`, searched recursively for the `packer-plugin-<name>_v<version>_x5.0_<os>_<arch>` files that `packer plugins install` writes), then `PATH`. The source type is `<plugin>-<builder>` as in Packer (`xenserver-iso`); the longest plugin name wins, then the first directory, then the highest version. `describe` checks the API version (`x5.*`) and the builder.
- **Protocol**: opendango starts `<plugin> start builder <name>` with the SDK's magic cookie, reads `5|<minor>|<network>|<address>` on stdout, connects, and uses `packer-plugin-sdk/rpc` as a client: `Prepare(settings, packer_* settings)`, then `Run(ctx, ui, hook)`. The plugin runs in the Dangofile directory: relative paths in its configuration are relative to it. Its stderr (its log) is kept, and its last lines are shown when it fails.
- **Config**: the FROM flags, unchanged (`--vm_name=x`). Values starting with `[` or `{` are JSON (lists, maps); the others are strings, converted by the plugin (`"2"` → int). The quotes left in FROM flag values are kept (BuildKit already removed the syntactic ones). Flags whose name contains password, secret or token are masked in the log and passed as `packer_sensitive_variables`. Pass secrets with `ARG` and `--build-arg`.
- **CONNECT** is translated to `communicator=ssh`, `ssh_username`, `ssh_port`, `ssh_timeout` (setting these as flags is an error); without CONNECT, `communicator=none`. The credentials (`ssh_password`, `ssh_private_key_file`, …) are FROM flags: there is no ephemeral key.
- **Flow**: the plugin creates the machine and connects; when it calls the `packer_provision` hook, opendango runs the stage instructions through the plugin's communicator (`packer.Comm`); then the plugin shuts down and produces its artifact, which opendango prints.
- **Limits**: a packer:// stage is a root and cannot be a parent (`FROM <packer stage>` is an error); `OUTPUT` is an error (the plugin configuration sets the artifact); `REBOOT` is not supported yet; it is never cached; `BREAKPOINT` pauses without an SSH command.
- **Errors**: `--on-error` becomes `packer_on_error` (abort → cleanup, keep → abort, ask → ask). With ask, a failed instruction offers retry; the plugin's own question after a failure is answered from that answer (keep → abort without cleanup), or asked.
- **Dependencies**: `packer-plugin-sdk` needs a go-cty with gob support: `go.mod` replaces `github.com/zclconf/go-cty` with `github.com/nywilken/go-cty v1.13.3` (MIT), like every Packer plugin (`packer-sdc fix`).
- `opendango` silences the standard `log` package, which the SDK uses for its internal messages.

## License constraints (mandatory)

- **Never copy code from Packer** (`hashicorp/packer`, under BSL 1.1 since version 1.10). The core must be written from scratch.
- `packer-plugin-sdk` (MPL-2.0) may be imported as a dependency. **Pin its version**: watch for a switch to BSL, and fork from the last MPL release if that happens.
- Third-party Packer plugins run as separate binaries and are never vendored into the code.

## Roadmap

1. **MVP**: parser, `validate`, `xo://` builder, SSH, `ARG`, `ENV`, `USER`, `WORKDIR`, `RUN`, `COPY`, `REBOOT`, `OUTPUT --as=template` and XVA.
2. ~~Multi-stage, `--target`, `BREAKPOINT`, `--on-error=ask`, parallel builds (`-j`), `fmt`~~ (done).
3. ~~`packer://` compatibility (builders), tested with `vatesfr/packer-plugin-xenserver`~~ (done); `REBOOT` in packer:// stages, `opendango plugins list|install`.
4. Native `BOOT` and `SERVE`, then more builders (QEMU, Proxmox, AWS, vSphere).
5. ~~Per-stage cache~~ (done); per-instruction caching based on snapshots, if the need is confirmed.

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
- The packer:// end-to-end test (`builders/packer/integration_test.go`) uses `vatesfr/packer-plugin-xenserver` and reads `XAPI_ENDPOINT`, `XAPI_USERNAME`, `XAPI_PASSWORD`, `PACKER_PLUGIN_PATH` and the xo:// variables (`OPENDANGO_TEST_POOL` and `OPENDANGO_TEST_NETWORK` required): the xva builder of the plugin cannot change the network of a cloned template, so the test first makes a temporary template on the DHCP network with xo://. It works around two bugs of the xva builder (v0.11.4) with `--firmware=bios` and `--installtimeout=10m`. The other packer:// tests build `builders/packer/testdata/packer-plugin-opendangotest`, a real plugin (same protocol) that runs the commands locally: no infrastructure needed.
- Executor unit tests (`internal/build`) use a recording builder and either a scripted communicator or the local `/bin/sh` in a sandbox where `sudo`, `shutdown` and `reboot` are stubs. Never let a unit test run a real `shutdown`, `reboot` or write outside its temporary directories.
- Validation errors point to the file and line: `Dangofile:12: ...`.
