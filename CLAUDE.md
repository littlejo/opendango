# CLAUDE.md

## Project

**opendango** is an open-source replacement for Packer. It builds VM images, but builds are described with **Dockerfile syntax** instead of HCL. The build file is called `Dangofile` (`*.dango` is also accepted).

- Language: **Go**
- License: **Apache-2.0**
- **Builders are Packer plugins** (`FROM plugin://<plugin>-<builder>`): opendango follows Packer's model (the plugin creates the machine and connects; the Dangofile instructions replace the provisioners) and shares Packer's plugin binaries and directory. The docs lead with plugins and stay builder-neutral; examples use a widely available plugin (`plugin://qemu`) rather than one infrastructure.
- A table of **known builders** (`builders/packer/known.go`) gives some plugins the full feature set (`OUTPUT`, `LABEL`, multi-stage, cache): `xenorchestra` (github.com/littlejo/packer-plugin-xenorchestra) first.
- `xo://` is a **native** Xen Orchestra builder (REST API `/rest/v0`), written first to prove the approach; `plugin://xenorchestra` is at parity with it and serves as the comparison. Removing the native core is not planned yet.
- Long-term: an OpenTofu-style plugin registry/mirror, so that `opendango plugin pull` does not depend on HashiCorp's servers (not started).

## Design principles

- **Keep the Dockerfile grammar unchanged.** Use the BuildKit parser (`github.com/moby/buildkit/frontend/dockerfile/parser`) at the AST level only. Do **not** use `frontend/dockerfile/instructions`, which rejects unknown keywords. Typing the instructions is our job.
- Flags follow the BuildKit form `--key=value`. For standard Dockerfile instructions (`FROM`, `RUN`, `COPY`, …) they come **before** positional arguments, as BuildKit requires. Custom instructions (`CONNECT`, `OUTPUT`, …) accept them anywhere on the line (`CONNECT ssh --user=debian`): BuildKit drops their arguments, so `internal/parser` re-reads the original line.
- Expand variables the Dockerfile way (`${VAR}`), not with Go templates.
- `FROM` carries both the builder and the source, as a URL: `FROM <builder>://<source> [AS <name>]`.
  - Examples: `plugin://qemu`, `plugin://xenserver-iso`, `xo://debian-12-cloud`.
  - `plugin://`, not `packer://`: Packer is a HashiCorp trademark, and the syntax must not suggest an official or affiliated product. Saying that opendango runs Packer plugins is fine in the docs.
  - If the name contains spaces, use a flag (`--template="..."`).
- Secrets come from the environment, never from the Dangofile: plugin flags take them through `ARG` and `--build-arg`. The XO variables are the ones of [xo-gocli](https://github.com/littlejo/xo-gocli): `XOA_ENDPOINT`, `XOA_TOKEN`, `XOA_INSECURE`.

## Instructions

| Instruction | Status | Semantics |
|---|---|---|
| `ARG` | MVP | Build variable, overridden with `--build-arg K=V`. Docker scoping: an ARG before the first `FROM` is only visible in `FROM` lines unless the stage declares it again (`ARG NAME`). |
| `FROM` | MVP | `FROM [--flags] <builder>://<source> [AS name]`. Flags configure the builder (xo: `--template`, `--pool`, `--iso`, `--sr`, `--network`, `--cpus`, `--memory`, `--disk`). |
| `FROM <stage>` | done | Multi-stage: starts from the image of an earlier stage (see *Multi-stage builds*). xo flags: `--cpus`, `--memory`, `--disk`, `--sr`, `--network` (not `--template`, `--pool` nor `--iso`). |
| `CONNECT` | MVP (ssh) | `CONNECT ssh --user=<user> [--port=22] [--timeout=10m]`. **Required** before any instruction that needs the VM (`RUN`, `COPY`, `ADD`, `WORKDIR`, `REBOOT`, `BREAKPOINT`, `ENV --persist`). An ephemeral ed25519 key is injected for `<user>` via cloud-init and removed from `authorized_keys` before shutdown. `winrm` is planned. |
| `RUN` | MVP | Shell, exec (JSON) and heredoc forms, run with `/bin/sh -c` (the shell form with `SHELL` when set). Not expanded at build time: the remote shell expands it, with the ARG and ENV values exported. |
| `COPY` | MVP | `COPY [--chmod=MODE] [--chown=U[:G]] <src>... <dest>`. Sources are relative to the Dangofile directory and must stay inside it (globs allowed). A directory source copies its contents. The destination is a directory when it ends with `/` or when there are several sources. Files are owned by the current `USER` unless `--chown` is set. |
| `ENV` | MVP | Visible to the following instructions **during the build only**; overrides an ARG of the same name. `ENV --persist` also writes `KEY="value"` to `/etc/environment` (as root). |
| `USER` | MVP | Defaults to the `CONNECT` user. Any other user runs through `sudo -n -H -u <user>`, so the connection user needs passwordless sudo. Groups are not supported. |
| `WORKDIR` | MVP | Created (`mkdir -p`) as the current `USER`, like BuildKit. A relative path is joined to the previous `WORKDIR`; without one, relative paths start from the connection user's home. |
| `REBOOT` | MVP | Runs `reboot` as root, then reconnects until `/proc/sys/kernel/random/boot_id` changes, within the `CONNECT` timeout. |
| `BREAKPOINT` | done | `BREAKPOINT [message]`. Needs `CONNECT`. Writes the ephemeral private key to a temporary file (mode 0600, removed when the build resumes or aborts), prints the `ssh` command for the `CONNECT` user and waits for Enter; Ctrl+C aborts the build (normal cleanup). Skipped with a message when stdin is not a terminal, so a forgotten `BREAKPOINT` does not block a CI build. |
| `OUTPUT` | MVP | Runs after all other instructions, once the VM is stopped: XVA exports first, then the template conversion. `OUTPUT --as=template [name]` (at most one) or `OUTPUT --format=xva [--compress=zstd\|gzip] <path>` (path relative to the Dangofile directory). Without `--as=template`, the VM is destroyed at the end. Other formats are planned. In a plugin:// stage, the settings of a known builder (see *Plugins*), an error otherwise. |
| `ADD` | done | `ADD [--chmod] [--chown] [--checksum=ALGO:HEX] <src>... <dest>`: `COPY` plus http(s) URL sources. A URL is downloaded by opendango (`net/http`, proxy from the environment) to a temporary file, then uploaded like `COPY`: mode 0600 unless `--chmod` (like Docker); the file name is the last element of the URL path when the destination is a directory (an error when it has none). `--checksum` (`sha256`, `sha384`, `sha512`) needs exactly one URL source and is checked after the download. Local sources are refused when they are a tar archive or a compressed file (gzip, bzip2, xz, zstd, by content): Docker would extract them, which is not supported yet; a directory is copied without checking its files. Git sources (`git@`, `git://`, `*.git`) are an error. Cache key: the arguments, the checksum and the local sources' content; a URL without `--checksum` makes the stage (and its children) not cached. |
| `LABEL` | done | `LABEL key=value ...` (and the legacy `LABEL key value`); keys and values are expanded. Each label becomes the tag `key=value` of the stage's `OUTPUT --as=template` template, added after the conversion and before the cache tag; a stage with `LABEL` needs `OUTPUT --as=template`. Inherited by child stages (a key set again replaces the value, in place); intermediate templates get none. Keys cannot start with `opendango-cache` (the cache tags) nor contain `=` or a newline. Failing to set a tag fails the stage (the template stays). Part of the cache key with the template name. In a plugin:// stage, the `tags` of a known builder (see *Plugins*), an error otherwise. |
| `BOOT` | done | `BOOT <keys>`, any number, before `CONNECT` and the instructions that need the VM (after `SERVE`). Typed in order into the VM console once the VM has started (`Builder.SendKeys`), with the `boot_command` syntax of Packer (`<enter>`, `<wait10>`, `<leftCtrlOn>`…): parsed, checked and typed by `packer-plugin-sdk/bootcommand`. The parser keeps the rest of the line as written (no flags; quotes, spaces and `--` are typed). Expansion keeps the quotes (`SkipProcessQuotes`); the escape character escapes `$` and itself. `${DANGO_HTTP_ADDR}` needs `SERVE`: it is a marker (`instructions.ServeAddrMarker`) until build time. Part of the cache key, with the marker. A failed `BOOT` ends the stage (no retry). |
| `SERVE` | done | `SERVE [--port=N] [--addr=IP] <dir>`, at most one per stage, before `CONNECT` and the instructions that need the VM. Serves `<dir>` (relative to the Dangofile directory, staying inside it) over HTTP from before the VM starts to the end of the stage; not inherited by child stages, not started on a cache hit. `RUN` gets `DANGO_HTTP_ADDR=<ip>:<port>` exported (the remote shell expands it); expanding `${DANGO_HTTP_ADDR}` anywhere else, or declaring it with `ARG`/`ENV`, is an error. The IP is `--addr`, or the builder's `LocalAddr` (xo: the local address on the route to `XOA_ENDPOINT`); the port is free unless `--port`. Listens on every interface, reads through `os.Root` (no symlink escape), logs every request. The directory content is part of the cache key. |
| `SHELL` | done | `SHELL ["executable", "parameters"...]`, JSON form only, not expanded (like Docker). The shell form of `RUN` becomes the SHELL with the script as its last argument; the exec form is not affected. The exports and the `cd` of `WORKDIR` still run in `/bin/sh`, which then `exec`s the SHELL. Needs no connection; inherited by child stages and replayed on a cache hit, like `USER`; part of the cache key. |

### Multi-stage builds

- `FROM <stage>` refers to an earlier stage by its `AS` name (no forward reference, names are unique).
- A stage inherits from its parent: `CONNECT` (it may set its own), `USER`, `SHELL`, `WORKDIR`, `ENV` and `LABEL`. `ARG` is not inherited, and `FROM` lines only see the global ARGs, like in Docker. Each stage gets its own ephemeral key: cloud-init sees a new instance because XO sets `instance-id` to the VM UUID.
- `opendango build` builds **every** stage, in file order; `--target X` builds X and its ancestors only.
- A stage that a selected stage starts from always ends as a template: its `OUTPUT --as=template`, or an intermediate template. With the cache (default), the intermediate template is `opendango-cache-<stage>-<unix time>` and stays (see *Cache*); with `--no-cache` (or when the stage is not cacheable), it is a temporary `opendango-tmp-<stage>-<unix time>` deleted at the end of the build (children first). A stage nobody starts from and without `OUTPUT` triggers a warning: its VM is destroyed.
- On failure the build stops: finished `OUTPUT`s (templates, XVA) and cached templates of the previous stages are kept, so the next build resumes from them; the failing VM and the temporary templates are deleted, unless `--on-error=keep` (or the answer `k` to `--on-error=ask`).
- Stages run one at a time by default (`-j 1`). With `-j N`, up to N stages run at the same time; a stage starts once its parent is done, in file order. After a failure, no new stage starts and the running ones finish (their `OUTPUT`s are kept); Ctrl+C stops everything. All failures are reported. Log lines are prefixed with the stage name, and `BREAKPOINT` / `--on-error=ask` questions are asked one at a time.

### Cache

The cache works per stage: the stages are the layers. It is on by default; `opendango build --no-cache` neither reads nor fills it.

- **Key** (`internal/build/cache.go`): a sha256 of the parent's key, the expanded `FROM` (builder, source or stage, flags), `CONNECT` (protocol, user, port), every step with its expanded values (`ARG`, `ENV`, `USER`, `SHELL`, `WORKDIR`, `RUN` script, `COPY` and `ADD` arguments plus the path, mode and content of every local source file, the `ADD --checksum`), the `SERVE` directory and its content (not its port or address) the `OUTPUT --as=template` name and the labels. Line numbers and layout are left out. `cacheVersion` is bumped when the key changes meaning.
- **Not cached**: a plugin:// stage of a builder that is not in the table (see *Plugins*); a stage with a `BREAKPOINT`, and its descendants (manual changes are not reproducible); a stage with an `ADD` of a URL without `--checksum`, and its descendants; a stage in which `--on-error=ask` retried an instruction; a stage that does not end as a template.
- **Limit**: the base template is keyed by the name written in `FROM`; a template replaced under the same name is not detected (like a Docker tag). Use `--no-cache`, or the template UUID.
- **Storage** (XO tags): every cached template has `opendango-cache:<key>`; intermediate ones also have `opendango-cache-created:<unix time>`. The lookup is in the pool of the template the VM would start from. On a hit, the stage starts no VM: its children start from the cached template, its `XVA` outputs are exported from it (`GET /vm-templates/{id}.xva`), and the final `USER`, `SHELL`, `WORKDIR`, `ENV` are replayed from the instructions.
- **Cleanup**: `opendango cache prune [--older-than=DURATION] [--dry-run]` deletes the intermediate cache templates (with the creation tag) only, never the `OUTPUT` templates. Nothing is deleted automatically.
- Per-instruction caching (snapshots) is not done: there is no documented way to create a VM from a snapshot in the REST API, snapshots belong to the build VM, and VHD chains are limited to about 30 snapshots.

`EXPOSE`, `HEALTHCHECK`, `VOLUME`, `CMD`, `ENTRYPOINT`, `ONBUILD` and `STOPSIGNAL` fail with `<X> has no meaning for a VM image`. They are reported by `validate`.

## CLI

Implemented (standard library `flag` package; move to cobra only if the CLI grows):

```
opendango init     [-f Dangofile] [--config=dango.yaml] [--force] [<source>...]
opendango build    [-f Dangofile] [--config=dango.yaml] [--build-arg K=V]... [--target X] [-j N] [--no-cache] [--on-error=abort|keep|ask]
opendango validate [-f Dangofile] [--config=dango.yaml] [--build-arg K=V]... [--offline]
opendango fmt      [-f Dangofile] [-w | --check]
opendango cache prune [--older-than=DURATION] [--dry-run]
opendango plugin pull <name|github.com/owner/name>[@version]...
opendango plugin search <term>
opendango plugin list
opendango plugin describe <source> [--search <text>]
opendango plugin rm <name|github.com/owner/name>[@version]...
opendango version
```

`--on-error=ask` needs a terminal on stdin (checked with `golang.org/x/term`; `opendango build` exits with code 2 otherwise). When an instruction fails, it prints the `ssh` command (if the VM was reached and the key is still installed) and asks `[r]` retry the instruction (after a new connection), `[c]` clean up (like `abort`) or `[k]` keep (like `keep`). Failures outside the instructions (start, connection, stop, `OUTPUT`, a later stage's builder) offer only `c` and `k`. End of input and Ctrl+C mean `c`. The returned error is the instruction's, whatever the answer.

`opendango fmt` (`internal/format`) prints the canonical layout, like gofmt; `-w` rewrites the file, `--check` exits with code 1 when it differs (the CI runs it on `examples/`). It only changes the layout: upper-case keywords with one space before the arguments, no trailing spaces, at most one blank line between instructions, one blank line before each `FROM` but the first (above its comments), a final newline. Arguments, continuation lines and heredoc bodies are never changed; the tests check that the formatted file parses to the same instructions and that formatting is stable.

`opendango plugin` (`builders/packer/registry.go`) installs Packer plugins:
- **Resolution**: a name alone is the official HashiCorp plugin of that name (the `packer-plugin-*` products of `api.releases.hashicorp.com/v1/products`), or else the only GitHub repository named `packer-plugin-<name>` that is not a fork (GitHub search API); several matches are an error that lists the sources. A source `github.com/<owner>/<name>` is the repository `<owner>/packer-plugin-<name>`, like in Packer.
- **Download**: official plugins come from `api.releases.hashicorp.com/v1/releases/packer-plugin-<name>/<latest|version>` (they are not published on GitHub); the others from the GitHub release (`releases/latest` or `releases/tags/v<version>`), asset `packer-plugin-<name>_v<version>_x5.<n>_<os>_<arch>.zip`. The zip is checked against the release's `SHA256SUMS` (no GPG signature check: it would need a dependency); it must hold exactly one `packer-plugin-<name>_v<version>_x5.<n>_<os>_<arch>` binary, written with mode 0755 next to a `_SHA256SUM` file (its hex sha256), then `describe` must succeed, or both are removed.
- **Layout**: Packer's plugin directory (`PluginDir`: `$PACKER_CONFIG_DIR/plugins`, or `~/.config/packer/plugins`), `github.com/<owner>/<name>/`, like `packer plugins install`: the discovery finds them, and Packer and opendango share them. An installed version is kept as is. `rm` only touches that directory.
- **search**: the official plugins whose name contains the term, then the GitHub repositories (`packer-plugin <term> fork:true in:name`) named `packer-plugin-*` with the term in the name, the most starred first; archived repositories and forks without a star are left out.
- `GITHUB_TOKEN` is sent to the GitHub API only (never to the downloads). Without it, the search API allows 10 requests a minute: the error says so.

## Architecture

```
cmd/opendango/              CLI (standard library flag)
internal/parser/            BuildKit parser → flat list of commands (recovers custom instruction args)
internal/instructions/      one type per instruction, parsing and validation
internal/plan/              ARG/ENV scoping, variable expansion, stage-level rules → Plan
internal/format/            opendango fmt: canonical layout of a Dangofile
internal/build/             executor: runs a Plan with a Builder and a Communicator
internal/communicator/ssh/  SSH commands and SFTP uploads
builders/packer/            Packer plugins (plugin://): discovery, installation, protocol, known builders, data sources, post-processors
builders/xo/                native Xen Orchestra builder (xo://)
```

Planned: `internal/communicator/winrm`, `internal/output` (qcow2, raw, vmdk, ova, checksums).

The `Builder` interface lives in `internal/build`: `Start(cloudConfig)`, `Address`, `Stop(grace)`, `Output`, `Destroy`, `ID`, `FindCache(key)`, `Tag(tag)`, `LocalAddr`, `SendKeys(keys)`. It grows with the roadmap, not before. Builders are selected by a plain `switch` on the `FROM` scheme (`build.NewBuilder`, `plan.checkBuilder`).

### Dependencies

| Module | Why |
|---|---|
| `github.com/moby/buildkit` (dockerfile `parser` and `shell`) | The Dockerfile grammar and the Docker variable expansion, unchanged. |
| `golang.org/x/crypto/ssh` | SSH client. |
| `github.com/pkg/sftp` | `COPY` uploads. |
| `github.com/coder/websocket` | XO JSON-RPC (conversion to a template, boot order, CD eject) and the VM console (`BOOT`). |
| `github.com/hashicorp/packer-plugin-sdk` v0.6.13 (pinned, MPL-2.0) | The client side of the Packer plugin protocol (`rpc`) for plugin://, and `bootcommand` (the `BOOT` syntax and its VNC driver). Requires the `go-cty` replace below. `bootcommand` brings `golang.org/x/mobile/event/key` (BSD, key constants of its USB driver). |
| `github.com/nywilken/go-cty` v1.13.3 (MIT, `replace` of `zclconf/go-cty`) | go-cty with gob encoding, required by the SDK's `rpc` package. |
| `go.yaml.in/yaml/v3` | Reads `dango.yaml`. Already in the build through `packer-plugin-sdk`. |
| `github.com/hashicorp/hcl/v2`, `github.com/zclconf/go-cty` | The settings spec of a plugin (`hcldec`) and the outputs of its data sources (`cty`). Already required by `packer-plugin-sdk`. |
| `golang.org/x/term` | Detects a terminal on stdin for `--on-error=ask` and `BREAKPOINT` (`/dev/null` is a character device too, so `os.ModeCharDevice` is not enough). Already required by `x/crypto`. |

The Vates SDK (`xenorchestra-go-sdk`) was evaluated and not used: opendango needs about 8 endpoints, and the SDK covers neither the streamed XVA export nor the conversion to a template, while adding many dependencies.

### Plugins (`plugin://`)

`FROM [--flags] plugin://<plugin>-<builder> [AS name]` (`builders/packer`, `internal/build/packer.go`). Builders run the stages; the data sources and post-processors of a known builder's plugin serve the cache and `OUTPUT --format=xva` (no provisioners: the instructions are opendango's).

- **Discovery**: `packer-plugin-<name>` binaries in `PACKER_PLUGIN_PATH`, then Packer's plugin directory (`$PACKER_CONFIG_DIR/plugins` or `~/.config/packer/plugins`, searched recursively for the `packer-plugin-<name>_v<version>_x5.0_<os>_<arch>` files that `packer plugins install` and `opendango plugin pull` write), then `PATH`. The source type is `<plugin>-<builder>` as in Packer (`xenserver-iso`); the longest plugin name wins, then the first directory, then the highest version. `describe` checks the API version (`x5.*`) and the builder.
- **Protocol**: opendango starts `<plugin> start builder|datasource|post-processor <name>` with the SDK's magic cookie, reads `5|<minor>|<network>|<address>` on stdout, connects, and uses `packer-plugin-sdk/rpc` as a client: `Prepare(settings, packer_* settings)`, then `Run(ctx, ui, hook)`. The plugin runs in the Dangofile directory: relative paths in its configuration are relative to it. Its stderr (its log) is kept, and its last lines are shown when it fails.
- **Config**: the FROM flags, unchanged (`--vm_name=x`). Values starting with `[` or `{` are JSON (lists, maps); the others are strings, converted by the plugin (`"2"` → int). The quotes left in FROM flag values are kept (BuildKit already removed the syntactic ones). Flags whose name contains password, secret or token are masked in the log and passed as `packer_sensitive_variables`. Pass secrets with `ARG` and `--build-arg`.
- **BOOT** is translated to `boot_command` (one entry per line, `${DANGO_HTTP_ADDR}` → `{{ .HTTPIP }}:{{ .HTTPPort }}`; setting `boot_command` as a flag is an error).
- **CONNECT** is translated to `communicator=ssh`, `ssh_username`, `ssh_port`, `ssh_timeout` (setting these as flags is an error); without CONNECT, `communicator=none`.
- **SERVE** is translated to `http_directory` (absolute path) and, with `--port`, `http_port_min` = `http_port_max` (setting these as flags is an error): the plugin serves the directory (it does not log the requests), and `boot_command` uses `{{ .HTTPIP }}:{{ .HTTPPort }}`. `RUN` gets `DANGO_HTTP_ADDR` from the hook data (`PackerHTTPAddr`); a warning when the plugin does not tell it. `--addr` needs a known builder with an `HTTPIP` setting (an error otherwise: vatesfr/xenserver has none). The credentials (`ssh_password`, `ssh_private_key_file`, …) are FROM flags, or the plugin's own (xenorchestra makes a temporary key).
- **Discovering and checking the settings**: plugins describe their settings through `ConfigSpec` (HCL2 spec over RPC): names and types only, no description, nothing marked required (Prepare tells). `opendango plugin describe <source> [--search]` (`packer.Settings`) lists them without the `packer_*` ones, notes those an instruction sets (`packer.Reserved`), and links the docs (`packer.DocURL`: developer.hashicorp.com for `github.com/hashicorp/*`, the GitHub repository otherwise, from the install path). `opendango validate` runs `build.CheckPlugins` unless `--offline`: per plugin:// stage, flags missing from the spec are errors with the closest name (`didyoumean.NameSuggestion` of the SDK), then the builder's `Prepare` with the settings the build would pass (`stageSettings`, `coreSettings`; a placeholder for a parent's template), which by contract creates nothing; a plugin that is not installed is a warning. The SDK's `ConfigSpec` client panics on a spec it cannot decode: `packer.Settings` recovers. The test plugin builds its spec from its `mapstructure` tags.
- **Settings file** (`internal/plan/settings.go`): `dango.yaml` next to the Dangofile (or `--config`) maps source types to settings. `plan.LoadSettings` turns YAML values into flag strings (scalars as text, lists and maps as JSON), refuses `packer.Sensitive` names (secrets come from the environment) and runs `packer.ParseConfig` on each section (source type, reserved settings, JSON). `plan.BuildSettings` gives a plugin:// stage its section (`Stage.FileFlags`), expanded with the global ARGs after escaping quotes and the escape character (like `literalFlags`), without the `Source`/`RootOnly` settings for a stage that starts from another one; `Stage.PackerFlags` merges it under the FROM flags, so the build, `CheckPlugins` (which says when an unknown setting comes from `dango.yaml`) and `cacheKey` (the effective flags of a plugin:// stage) see the same values. An unused section is a warning. `opendango init` writes every setting of the given sources (or the Dangofile's root plugin:// sources) commented out with a typed empty value and its type, without the `packer_*`, reserved and sensitive ones; `--force` overwrites.
- **Hook data** (`packer.HookData`): the provision hook receives Packer's generated data, which the RPC delivers as `map[any]any`: `PackerHTTPAddr`, `Host`, `Port`, `User`, `SSHPrivateKey` (the SDK's `ERR_*` placeholders mean unset). While the instructions run, `BREAKPOINT` and `--on-error=ask` print the `ssh` command of that connection (the key in a temporary 0600 file, removed afterwards; no `-i` without a key).
- **REBOOT** runs `reboot` as root through the plugin's communicator, then reads the boot ID until it changes (within the `CONNECT` timeout, one minute per attempt): the SDK's SSH communicator reconnects by itself when a command cannot open a session.
- **Flow**: the plugin creates the machine and connects; when it calls the `packer_provision` hook, opendango runs the stage instructions through the plugin's communicator (`packer.Comm`); then the plugin shuts down and produces its artifact, which opendango prints.
- **Known builders** (`builders/packer/known.go`): a table of the builders whose artifact opendango understands, by source type, so that their stages behave like native ones. For `xenorchestra` (github.com/littlejo/packer-plugin-xenorchestra): `OUTPUT --as=template <name>` → `template_name`; the labels, then the cache tags → `tags` (`key=value` list); `SERVE --addr` → `http_ip`; `BOOT` → `boot_wait=-1s` (typed at once, like native); `drop_inherited_tags` → the cache tag prefixes (XO copies the source template's tags to the VM); these settings cannot be FROM flags. `OUTPUT --format=xva [--compress] <path>` runs the plugin's `xva` post-processor (`packer.PostProcess`) on the artifact id after the build, once per OUTPUT (any number, each with its compression), or on the cached template. Without `OUTPUT --as=template`, opendango deletes the artifact (`packer.Build.DestroyArtifact`, or after the XVA exports through the kept plugin), like the destroyed VM of a native stage, with the same plan warning. For the other builders, `OUTPUT` and `LABEL` are errors that list the known ones. The test plugin stands for `xenorchestra` in the executor tests (with a store-backed `template` data source and an `xva` post-processor that writes a file); `TestXenorchestraEndToEnd`, `TestXenorchestraMultiStageEndToEnd` and `TestXenorchestraCacheEndToEnd` run the real plugin.
- **Multi-stage** (known builders with a `Source` setting): `FROM [--flags] <stage>` builds with the root's plugin and builder. Its flags (`plan.Stage.PackerFlags`, `packer.Known.StageFlags`) are its own plus the parent's `Inherited` ones it does not set (xenorchestra: `endpoint`, `token`, `insecure`); the `RootOnly` ones are errors (xenorchestra: `template`, `pool`, `iso`). The executor sets `Source` (xenorchestra: `template`) to the parent artifact's `Id()` and inherits `USER`, `SHELL`, `WORKDIR` and `ENV` like a native stage. A parent without `OUTPUT --as=template` gets `template_name=opendango-tmp-<stage>-<unix time>` and its plugin keeps running (`packer.Build.KeepPlugin`): the artifact can only be destroyed through the plugin that made it, which `Run` does at the end of the build with the native intermediate templates (`executor.template()`, children first); `--on-error=keep` keeps it and stops the plugin. A builder without `Source` cannot be a parent.
- **Cache** (known builders with `Cache`): the same key as a native stage (`cacheKey`, FROM flags included). The lookup runs the plugin's data source (`packer.Datasource`; xenorchestra: `template`, with `tags=[opendango-cache:<key>]`, `allow_missing`, `same_pool_as` = the stage's source: the root's `template` flag or the parent's id, and the `endpoint`, `token`, `insecure`, `pool` flags). A hit builds nothing: the children start from the cached template, the XVA OUTPUTs are exported from it, the final `USER`, `SHELL`, `WORKDIR`, `ENV` are replayed. A miss adds the cache tags to `tags` (an intermediate template: `opendango-cache-<stage>-<time>` with the creation tag first, kept); `opendango cache prune` finds them like the native ones (same XO, same tags). In a cached stage `--on-error=ask` offers no retry: the plugin sets the cache tags at the end, so a stage fixed by hand would be cached (use `--no-cache`).
- **Limits**: only the builders of the table behave like native ones; for the others, `OUTPUT` and `LABEL` are errors, a stage cannot be a parent and nothing is cached. `SERVE` requests are not logged (the plugin serves the directory).
- **Errors**: `--on-error` becomes `packer_on_error` (abort → cleanup, keep → abort, ask → ask). With ask, a failed instruction offers retry; the plugin's own question after a failure is answered from that answer (keep → abort without cleanup), or asked.
- **Dependencies**: `packer-plugin-sdk` needs a go-cty with gob support: `go.mod` replaces `github.com/zclconf/go-cty` with `github.com/nywilken/go-cty v1.13.3` (MIT), like every Packer plugin (`packer-sdc fix`).
- `opendango` silences the standard `log` package, which the SDK uses for its internal messages.

### Native Xen Orchestra builder (`xo://`)

- It goes through the XO REST API with a thin client (`builders/xo/client.go`). The route reference is served at `/rest/v0/docs` on each instance; the source is `@xen-orchestra/rest-api` in `vatesfr/xen-orchestra`.
- The REST API can neither convert a VM to a template nor set its boot order: those calls go through JSON-RPC (`session.signIn`, then `vm.convertToTemplate`, `vm.setBootOrder` or `vm.ejectCd`, on the `/api/` WebSocket, `builders/xo/jsonrpc.go`). Switch to REST when XO adds the endpoints.
- Templates live in the `vm-templates` collection. A default template's id is `<pool>-<uuid>`; `create_vm` takes the bare UUID. Names, ids and UUIDs are all accepted. The same template UUID can exist in several pools (XO Hub templates): `--pool` restricts the lookup, and a template found in several pools without `--pool` is an error.
- Requirements on the template: cloud-init (for the SSH key) and the guest tools (XO only reports the IP address through them). With `--iso`, the same requirements apply to the installed system.
- **Console** (`builders/xo/console.go`): the WebSocket `/api/consoles/{vm id}` of the XO web UI (cookie `token=<XOA_TOKEN>`, `operate` permission), which carries raw RFB to the VM's VNC console. The client does the RFB 3.8 handshake with security type None (XO checks the token) and only sends `KeyEvent` messages. One connection per `BOOT` line.
- **`--iso`**: the VM is created stopped with `install: {method: cdrom, repository: <ISO VDI>}` and `destroy_cloud_config_vdi: false` (XO would delete the drive at most 10 minutes after the start, during the installation). XO sets the boot order to CD first, and installers cannot always eject the ISO (Debian with UEFI cannot): the builder sets it to `cd` (disk first: the empty disk is skipped until the system is installed) with the JSON-RPC `vm.setBootOrder`, then starts the VM. A template without disks (no `$VBDs`, no `template_info.disks`) gets a new disk named after the VM and needs `--disk`; otherwise `--disk` resizes the template's disk 0. After the stop, `removeInstallMedia` ejects the ISO (JSON-RPC `vm.ejectCd`; deleting the CD VBD instead breaks `vm.convertToTemplate`, which ejects from XO's cache) and deletes the `XO CloudConfigDrive` VDI.
- Flow:
  1. `POST /pools/{id}/actions/create_vm?sync=true` with the template, CPU, memory, disk, network, cloud-config, `boot: true` and `destroy_cloud_config_vdi: true` (the cloud-config holds the SSH key);
  2. poll `GET /vms/{id}` until `mainIpAddress` (or an `addresses` IPv4) is set;
  3. connect over SSH and run the instructions;
  4. remove the ephemeral key, `shutdown -h now` from the guest, wait for `Halted`, then `clean_shutdown`, then `hard_shutdown`;
  5. export the XVA (`GET /vms/{id}.xva?compress=`) and/or rename (`PATCH`) and convert to a template;
  6. clean up: `DELETE /vms/{id}` unless the VM became the template. Cleanup happens **even on failure or Ctrl+C**, unless `--on-error=keep` is set or `k` is answered to `--on-error=ask`.
- `vm.convertToTemplate` returns before XO's object cache lists the template: the builder waits for `GET /vm-templates/{id}` before reporting the output done.
- `FROM <stage>` creates the VM from the parent's template (its UUID, in the root's pool). Once converted, `Destroy` deletes the template (`DELETE /vm-templates/{id}`), which is how temporary templates are removed. A builder that found its template in the cache (`FindCache`) never deletes it. Tags are added with `PUT /vm-templates/{id}/tags/{tag}`, then the builder waits until XO lists them (its object cache lags a moment behind the call, and the next stage may look up the cache right away). `create_vm` copies the template's tags to the VM: `Start` removes the copied cache tags (`DELETE /vms/{id}/tags/{tag}`, from the template's tags read before the creation), otherwise a cache lookup could return the child's template and `cache prune` could delete an `OUTPUT` template; a `key=value` tag (`LABEL`) replaces the copied tags of the same key.

## License constraints (mandatory)

- **Never copy code from Packer** (`hashicorp/packer`, under BSL 1.1 since version 1.10). The core must be written from scratch.
- `packer-plugin-sdk` (MPL-2.0) may be imported as a dependency. **Pin its version**: watch for a switch to BSL, and fork from the last MPL release if that happens.
- Third-party Packer plugins run as separate binaries and are never vendored into the code.

## Roadmap

1. **MVP**: parser, `validate`, `xo://` builder, SSH, `ARG`, `ENV`, `USER`, `WORKDIR`, `RUN`, `COPY`, `REBOOT`, `OUTPUT --as=template` and XVA.
2. ~~Multi-stage, `--target`, `BREAKPOINT`, `--on-error=ask`, parallel builds (`-j`), `fmt`~~ (done).
3. ~~`plugin://` compatibility (builders), tested with `vatesfr/packer-plugin-xenserver`, `opendango plugin pull|search|list|rm`, `REBOOT` in plugin:// stages, plugin://xenorchestra at parity with xo:// (table of known builders: OUTPUT, LABEL, multi-stage, cache)~~ (done).
4. ~~`SERVE`, `BOOT` and `FROM --iso`~~ (done); more builders in the table of known builders (QEMU first, with a CI job on GitHub runners; Proxmox, AWS, vSphere).
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
- Tests that need a real XO instance use the `integration` build tag and read `XOA_ENDPOINT`, `XOA_TOKEN`, `XOA_INSECURE`, `OPENDANGO_TEST_TEMPLATE`, `OPENDANGO_TEST_USER` (optional: `OPENDANGO_TEST_POOL`, `OPENDANGO_TEST_NETWORK`, `OPENDANGO_TEST_SR`, and `OPENDANGO_TEST_ISO` for the installation from an ISO). Run them with `go test -tags integration -timeout 60m ./builders/xo/`.
- The plugin:// end-to-end test (`builders/packer/integration_test.go`) uses `vatesfr/packer-plugin-xenserver` and reads `XAPI_ENDPOINT`, `XAPI_USERNAME`, `XAPI_PASSWORD`, `PACKER_PLUGIN_PATH` and the xo:// variables (`OPENDANGO_TEST_POOL` and `OPENDANGO_TEST_NETWORK` required): the xva builder of the plugin cannot change the network of a cloned template, so the test first makes a temporary template on the DHCP network with xo://. It works around two bugs of the xva builder (v0.11.4) with `--firmware=bios` and `--installtimeout=10m`. `TestPluginPullEndToEnd` (same file set, `integration` tag) installs the real `qemu` (releases.hashicorp.com) and `vatesfr/xenserver` plugins in a temporary directory: it needs the network only, and does not use the GitHub search (rate-limited). `TestXenorchestraEndToEnd` (`builders/packer/xenorchestra_integration_test.go`) builds with packer-plugin-xenorchestra (found by the discovery, skipped otherwise) and the xo:// variables: a stage with `LABEL`, an XVA and a named template, and a stage without `OUTPUT` whose template must be deleted. The registry unit tests serve both APIs with `httptest`. The other plugin:// tests build `builders/packer/testdata/packer-plugin-opendangotest`, a real plugin (same protocol) that runs the commands locally: no infrastructure needed.
- Executor unit tests (`internal/build`) use a recording builder and either a scripted communicator or the local `/bin/sh` in a sandbox where `sudo`, `shutdown` and `reboot` are stubs. Never let a unit test run a real `shutdown`, `reboot` or write outside its temporary directories.
- Validation errors point to the file and line: `Dangofile:12: ...`.
