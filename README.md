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
