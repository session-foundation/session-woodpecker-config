# session-woodpecker-config

A [Woodpecker CI configuration extension][config-ext] that lets Session projects write their
pipelines in [jsonnet] or [Starlark] instead of plain YAML.

Woodpecker fetches a repository's pipeline config and POSTs it to this service, which evaluates
`*.jsonnet` / `*.star` files into one Woodpecker workflow per pipeline. Plain YAML configs are passed
through unchanged.

Legacy `.drone.jsonnet` files are also accepted during the migration away from Drone CI: their Drone
pipelines are translated to Woodpecker workflows on the fly, and a `DEPRECATED` workflow is added to
flag that the repository still needs migrating.

[config-ext]: https://woodpecker-ci.org/docs/usage/extensions/configuration-extension
[jsonnet]: https://jsonnet.org/
[Starlark]: https://github.com/bazelbuild/starlark

## Writing configs

A repository uses whichever of these Woodpecker finds first (see the server configuration below):

1. `.woodpecker/` — every `*.yaml`, `*.yml`, `*.jsonnet` and `*.star` file directly inside it
2. `.woodpecker.jsonnet`, `.woodpecker.star`, `.woodpecker.yaml` or `.woodpecker.yml`
3. `.drone.jsonnet` (deprecated)

If any config files in `.woodpecker/` have names starting with `override` (`override.star`,
`override-lint.jsonnet`, ...), only those are used, and the rest of `.woodpecker/` is ignored. This
is for branches that merge a project's own configs but need to run something else, such as Debian
packaging branches: they add their overrides and keep the project's files unchanged, so merges don't
conflict.

A jsonnet or Starlark config produces either a list of workflows, each with a `name`, or a single
workflow, which is named after the file. Apart from `name`, a workflow is exactly what you would
write in a [Woodpecker YAML workflow][syntax]. Each workflow runs separately, on an agent matching
its `labels`, and shows up as its own status check on the forge.

[syntax]: https://woodpecker-ci.org/docs/usage/workflow-syntax

A jsonnet config is evaluated as-is; if its top level is a function, it is called with the build
context as `ctx`:

```jsonnet
function(ctx) [
  {
    name: 'Debian sid (amd64)',
    labels: { platform: 'linux/amd64' },
    when: [{ event: ['push', 'pull_request'] }],
    steps: [{ name: 'build', image: 'debian:sid', commands: ['echo building ' + ctx.repo.full_name] }],
  },
]
```

A Starlark config must define `main(ctx)`, where `ctx` fields are accessed as attributes. The `json`
module and `struct()` are available.

```python
def main(ctx):
    return [{
        "name": "Debian sid (amd64)",
        "labels": {"platform": "linux/amd64"},
        "when": [{"event": ["push", "pull_request"]}],
        "steps": [{"name": "build", "image": "debian:sid", "commands": ["echo building " + ctx.repo.full_name]}],
    }]
```

`ctx.repo` and `ctx.pipeline` are the repository and pipeline objects Woodpecker sends to
extensions (`ctx.repo.full_name`, `ctx.pipeline.event`, `ctx.pipeline.branch`, ...).

Some things to be aware of:

- Imports (jsonnet `import`/`importstr`, Starlark `load()`) are not supported: a config must be
  self-contained.
- A `/` in a workflow name is replaced with `: ` (so `Debian sid/Debug` becomes `Debian sid:
  Debug`), because Woodpecker names workflows after the base name of their config file and would
  otherwise truncate the name at the last `/`.
- Workflow names must be unique across all of a repository's config files.
- Workflows appear in the order the config lists them. Woodpecker sorts workflows by config file
  name, so the generated files are named `<config file>/<index>/<workflow name>.yaml`, and since
  restarting a pipeline only has the stored workflows, each also starts with a
  `# session-woodpecker-config order:` comment that restores the order.
- Pull request pipelines get one more workflow, `All builds`, which depends on all the others and
  only runs if they all succeed.  GitHub can only require status checks by exact name, and
  Woodpecker reports one per workflow, so require `ci/woodpecker/pr/All builds` in branch rules
  instead of listing every workflow.  If any workflow fails it is skipped, which GitHub shows as
  pending.  Steps with `failure: ignore` don't fail their workflow, so they don't stop it.  Plain
  YAML configs don't get it, since they are passed through unchanged.
- Woodpecker substitutes `${VAR}` references in the generated config before running it, so shell
  variables must be written `$${VAR}` (or as bare `$VAR`, which Woodpecker leaves alone).
- Evaluation is limited to 5 seconds and 1 GiB of memory by default; errors, including exceeding
  those limits, are reported as the pipeline's error in Woodpecker.

## `.drone.jsonnet` translation

Only the parts of Drone's pipeline format used by Session projects are supported: `docker` and
`exec` pipelines, `platform`, `node`, `environment`, `services`, `trigger`, `depends_on`, and steps
with `image`, `commands`, `environment` (including `from_secret`), `pull`, `failure`, `depends_on`,
`when`, `settings`, `detach` and `privileged`. Anything else is rejected with an error rather than
translated into something that might behave differently.

Translated workflows clone the repository the way Drone did, fully and without submodules, rather
than with Woodpecker's default shallow partial clone that also checks out submodules. The configs
handle submodules themselves, and their `git fetch --tags` breaks in a partial clone.

Drone left a variable empty when its `from_secret` secret was missing or not available to the
build, and scripts rely on that (for instance by skipping an upload when `SSH_KEY` is empty), but
Woodpecker fails the whole pipeline instead. So the translation only keeps `from_secret` for
builds that should have the secrets: push, tag, deployment, cron and manual builds of repositories
listed in the secret repository file (see below). For pull requests and all other repositories the
variables are left unset. Create the secrets as organization (or user) secrets allowed for those
events; a secret that is missing where it is expected still fails the pipeline.

In native configs, the Woodpecker way is to put the secret only on a step that runs where the
secret exists, since Woodpecker only resolves secrets for steps that will run:

```yaml
- name: upload
  image: debian:stable-slim
  environment: { SSH_KEY: { from_secret: SSH_KEY } }
  commands: [ ./utils/ci/upload.sh ]
  when: [{ event: push, repo: 'session-foundation/*' }]
```

`${DRONE_*}` references in the config are rewritten to the equivalent Woodpecker variables, and
each step starts by exporting the `DRONE_*` environment variables Drone set (from the equivalent
`CI_*` ones), since the scripts steps run read them directly and Woodpecker only sets them for
plugins. See `internal/drone/testdata/*.golden.yaml` for what real configs translate to.

## Migrating from `.drone.jsonnet`

Replace `.drone.jsonnet` with `.woodpecker/build.jsonnet` (or a Starlark `.woodpecker/build.star`),
in the format described under [Writing configs](#writing-configs). The translation above is a
working model: the golden files in `internal/drone/testdata/` show what each Drone construct
becomes. The things that need changing:

**The pipeline itself**

- `kind` and `type` go away. A `docker` pipeline becomes `labels: { backend: 'docker' }`. An
  `exec` pipeline becomes `labels: { backend: 'local' }`, and each step gets `image: 'sh'`, which
  the local backend uses as the shell.
- `platform: { os, arch }` becomes `labels: { platform: 'linux/amd64' }` (or `darwin/arm64`, ...).
  Always set both `platform` and `backend`: a workflow without them can be scheduled on any agent,
  including the macOS ones, which run steps directly on the host.
- `node` entries become further labels.
- `trigger` becomes `when`, a list of conditions. Drone's `event: { exclude: [...] }` has to be
  written as the explicit list of events wanted; Drone's `promote`/`rollback` events are
  `deployment` and `custom` is `manual`.
- Put Drone's pipeline-level `environment` on each step, as the translation does.
- Write `: ` where a Drone pipeline name has `/`. A `/` does not work in a Woodpecker workflow
  name, which is taken from the base name of its config file and so would be cut at the last `/`.
  The translation rewrites it to `: ` for that reason, so `Debian sid/Debug` has been running as
  `Debian sid: Debug`: spelling it that way keeps the workflow, and the status check it reports,
  under the name it already has. (The service would rewrite a `/` in a native config too, but the
  config should say the name it gets.) Don't drop the separator instead: that renames the workflow.

**Cloning**

Drone did a full clone without submodules. Woodpecker's default is a shallow, treeless partial
clone that also checks out all submodules (depth 1). So the usual Drone step of
`git submodule update --init --recursive --depth=1` can simply be dropped. If the build needs tags
(for `git describe`, say), set:

```jsonnet
clone: [{ name: 'clone', image: 'woodpeckerci/plugin-git:2', settings: { tags: true } }],
```

Fetching tags also turns off the partial clone. Don't keep a `git fetch --tags` of your own with
the default clone: in a partial clone with submodules checked out it fails with "upload-pack: not
our ref".

**Secrets**

A `from_secret` that is missing, or not allowed for the event, fails the whole pipeline, and pull
requests never get the (upload) secrets. So instead of relying on an empty `SSH_KEY`, only attach
the secret to steps limited to where it exists, as in the upload example above.

**Variables, in the config and in scripts**

Woodpecker's variables are the `CI_*` ones; it sets the `DRONE_*` ones only for plugins, never for
commands. In the config, replace `${DRONE_*}` references (see `compileTimeVars` in
`internal/drone/vars.go` for the equivalents). Just as importantly, **scripts run by the steps**
(upload scripts, packaging scripts, ...) that read `DRONE_*` from the environment must be changed
to read the `CI_*` variables, or the step must pass them explicitly: without the translation they
are simply unset. `grep -rn DRONE_` in the repository finds them. The common ones:

| Drone | Woodpecker |
|---|---|
| `DRONE_COMMIT`, `DRONE_COMMIT_SHA` | `CI_COMMIT_SHA` |
| `DRONE_BRANCH` | `CI_COMMIT_BRANCH` |
| `DRONE_TAG` | `CI_COMMIT_TAG` |
| `DRONE_PULL_REQUEST` | `CI_COMMIT_PULL_REQUEST` |
| `DRONE_REPO` | `CI_REPO` |
| `DRONE_BUILD_EVENT` | `CI_PIPELINE_EVENT` |
| `DRONE_BUILD_NUMBER` | `CI_PIPELINE_NUMBER` |
| `DRONE_BUILD_CREATED` | `CI_PIPELINE_CREATED` |
| `DRONE_WORKSPACE` | `CI_WORKSPACE` |
| `DRONE_STAGE_MACHINE` | `CI_MACHINE` |

`DRONE_STAGE_OS` and `DRONE_STAGE_ARCH` have no equivalent; the config knows the platform, so set
them (or better names) in the step's `environment` where scripts need them.

**Hosts and images**

The oxen.rocks hosts are deprecated, so a migration should also move off them, in the config and in
the scripts it runs (`grep -rn oxen.rocks` finds them):

- `registry.oxen.rocks/` images become `registry.session.codes/`, with the same image names.
- An image name with a `lokinet-ci-` prefix (`registry.oxen.rocks/lokinet-ci-debian-sid`, say) is no
  longer supported: drop the prefix (`registry.session.codes/debian-sid`).
- `oxen.rocks` for dependency downloads and build uploads becomes `builds.session.codes`: for
  example `-DLOCAL_MIRROR=https://builds.session.codes/deps`, and upload scripts' `sftp` to
  `drone@builds.session.codes` with upload paths under `builds.session.codes/` instead of
  `oxen.rocks/`.
- The `deb.oxen.io` apt repository is now preferably `deb.session.foundation`: the same repository
  and signing key under a different name, so only the domain changes, in sources entries and in
  key URLs (`https://deb.session.foundation/pub.gpg`).

**Restructure, don't transliterate**

The jsonnet helpers translate almost mechanically to Starlark functions. Resist stopping there: a
line-for-line port keeps years of Drone-era accretion (a function parameter per option, free-form
"extra" strings, copy-pasted platform variants, workarounds nobody remembers). The migration is the
cheapest time to clean that up. Some things that usually help:

- **Make build options data.** Instead of `lto=`, `werror=`, `build_tests=`, ... parameters plus a
  `cmake_extra` string, keep a dict of default options that each build overrides, rendered into
  `-D` flags in one place (`True`/`False` to `ON`/`OFF`). Builds then say only what is different
  about them. Drop options that just restate the build system's own defaults.
- **Share the build sequence across platforms.** Linux (docker) and macOS (local) workflows tend to
  differ only in setup (installing packages, or exporting `SDKROOT`) and labels; the
  configure/build/test/package commands can be one function that both use.
- **Let the data imply the steps.** For example, a build that is given packaging commands also gets
  the upload step, rather than needing a separate `upload=True` flag that has to be kept in sync.
- **One `workflow()` helper** for `labels`, `when` and `clone`, so per-workflow settings aren't
  repeated in every builder function.
- **Delete dead code** rather than porting it: commented-out pipelines, parameters no caller uses,
  and scripts that only those referenced. Git history keeps them.
- **Prefer what the environment already knows** in scripts, e.g. `uname -s` rather than a
  replacement for `DRONE_STAGE_OS`.

Starlark has no f-strings, so use `%` formatting; `x if cond else y`, `dict(base, **overrides)`,
`d.update()`, list comprehensions and `type(v) == "bool"` cover most of what jsonnet configs do.

Keep the restructuring behaviour-neutral, and make deliberate behaviour changes (different
parallelism, script semantics, ...) in separate commits. That way the conversion can be checked by
rendering both configs and diffing the YAML: a throwaway test in this repository that evaluates the
old file with `eval.Jsonnet` and `drone.Translate` and the new one with `eval.Starlark` and
`workflow.FromResult`, then prints each workflow's `YAML()`, is enough. The only differences should
be the intended ones: clone settings, the `DRONE_*` exports, secrets moved to their own steps, and
so on.

**Afterwards**

Once `.drone.jsonnet` is gone, the `DEPRECATED` workflow no longer appears. Pull request pipelines
from jsonnet and Starlark configs still get the `All builds` workflow, so branch rules requiring
`ci/woodpecker/pr/All builds` keep working. Plain YAML configs don't get it.

## Running the service

Build a static binary (the binary re-executes itself to evaluate configs, so it is the only file
needed) and install it:

```
make
install session-woodpecker-config /usr/local/bin/
```

Fetch the public key of the Woodpecker server (which must already be running, since it generates
the key on first start), then install and start the systemd unit from `contrib/`:

```
mkdir -p /etc/session-woodpecker-config
curl -o /etc/session-woodpecker-config/woodpecker-key.pem https://ci.example.org/api/signature/public-key
install -m 644 contrib/secret-repos /etc/session-woodpecker-config/
install -m 644 contrib/session-woodpecker-config.service /etc/systemd/system/
systemctl daemon-reload
systemctl enable --now session-woodpecker-config
curl 'http://[::1]:7776/healthz'
```

Every request must be signed by the Woodpecker server whose public key is given, and the service
holds no secrets and needs no network access of its own, so it can safely be exposed publicly. The
recommended setup is to expose it through the reverse proxy in front of Woodpecker, which provides
TLS (whoever can alter its responses can inject pipeline steps) and means the default
`WOODPECKER_EXTENSIONS_ALLOWED_HOSTS`, which refuses loopback and private addresses, can stay in
place. The proxy must pass the request path through unchanged, since it is covered by the request
signature; with nginx, that means a `proxy_pass` without a path:

```nginx
location = /config-extension {
    proxy_pass http://[::1]:7776;
}
```

`/etc/session-woodpecker-config/secret-repos` lists the repositories whose `.drone.jsonnet`
pipelines are given secrets, as `owner/name` patterns such as `session-foundation/*`, one or more
per line, with `#` comments. The service notices when it changes, so edits apply from the next
pipeline without a restart. A missing file means no repository gets secrets, and a broken edit is
logged and the previous list kept.

See `-help` for resource limits and other options.

The workflows the service adds that do next to nothing (`All builds` and `DEPRECATED`) carry the
label `trivial: yes`, so that a small dedicated Docker agent can run them without waiting for a free
build slot, and they can't be scheduled on the build agents at all:

```
WOODPECKER_AGENT_LABELS=!trivial=yes
WOODPECKER_MAX_WORKFLOWS=4
WOODPECKER_BACKEND_DOCKER_LIMIT_MEM=16777216
```

(The `!` makes the agent take only workflows with that label.)

Woodpecker server configuration:

```
WOODPECKER_CONFIG_EXTENSION_ENDPOINT=https://ci.example.org/config-extension
WOODPECKER_DEFAULT_PIPELINE_CONFIGS=.woodpecker/,.woodpecker.jsonnet,.woodpecker.star,.woodpecker.yaml,.woodpecker.yml,.drone.jsonnet
WOODPECKER_DEFAULT_PIPELINE_CONFIG_EXTENSIONS=.yaml,.yml,.jsonnet,.star
```

Leave per-repository config paths unset, since they replace the default search order entirely.
