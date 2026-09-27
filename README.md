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

`${DRONE_*}` references are rewritten to the equivalent Woodpecker variables. See
`internal/drone/testdata/*.golden.yaml` for what real configs translate to.

## Running the service

Build a static binary (the binary re-executes itself to evaluate configs, so it is the only file
needed) and install it:

```
CGO_ENABLED=0 go build -trimpath ./cmd/session-woodpecker-config
install session-woodpecker-config /usr/local/bin/
```

Fetch the public key of the Woodpecker server (which must already be running, since it generates
the key on first start), then install and start the systemd unit from `contrib/`:

```
mkdir -p /etc/session-woodpecker-config
curl -o /etc/session-woodpecker-config/woodpecker-key.pem https://ci.example.org/api/signature/public-key
install -m 644 contrib/session-woodpecker-config.service /etc/systemd/system/
systemctl daemon-reload
systemctl enable --now session-woodpecker-config
curl http://127.77.0.1:8124/healthz
```

Every request must be signed by the Woodpecker server whose public key is given. The service holds
no secrets and needs no network access of its own, so it can safely be exposed publicly, but use
TLS if Woodpecker reaches it over a network: whoever can alter its responses can inject pipeline
steps. See `-help` for resource limits and other options.

Woodpecker server configuration:

```
WOODPECKER_CONFIG_EXTENSION_ENDPOINT=http://127.77.0.1:8124/config
WOODPECKER_EXTENSIONS_ALLOWED_HOSTS=127.77.0.1/32
WOODPECKER_DEFAULT_PIPELINE_CONFIGS=.woodpecker/,.woodpecker.jsonnet,.woodpecker.star,.woodpecker.yaml,.woodpecker.yml,.drone.jsonnet
WOODPECKER_DEFAULT_PIPELINE_CONFIG_EXTENSIONS=.yaml,.yml,.jsonnet,.star
```

`WOODPECKER_EXTENSIONS_ALLOWED_HOSTS` is only needed for a non-public address: by default Woodpecker
refuses to contact extensions on loopback or private addresses. Leave per-repository config paths
unset, since they replace the default search order entirely.
