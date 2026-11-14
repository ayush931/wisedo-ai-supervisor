# Rule 00 — Product independence

Source: `AGENTS.md`, `README.md`, `docs/independent-supervisor.md` §1, WIS-146, WIS-290, WIS-270/271/272 "Delivery boundary".

`Wisedo-Technology/wisedo-ai-supervisor` is an independent product. It preserves Git
history from the earlier OEE component but inherits no runtime dependency on anything else.

## Required

- The application must boot and pass its gates with MES, PlantOps, Sight and Flow absent.
- Own the application database, role, migrations, object prefixes, OIDC realm and Temporal
  namespace. Keycloak and Temporal use databases/roles separate from application data.
- Read `README.md` and `docs/independent-supervisor.md` before changing product scope.

## Forbidden

- Importing MES private SDKs, querying an MES database, or requiring another WiseDo product,
  gateway, broker, shared identity or release pipeline to boot this application.
- Treating the repository rename, scaffolding or fixture tests as a delivered Supervisor.
- Introducing Redis as a correctness prerequisite, a second workflow scheduler, or a
  microservice per `internal/` package. The packages are one Go application.
- Making Graphiti, Neo4j, PM4Py, Nango, Airbyte or an enterprise Langfuse edition a release
  dependency. An optional projection can never authorize a fact or an action.

The retained OEE component (`internal/oee`) is a component contract inside this repository,
not a Supervisor dependency. The legacy `wisedo_plantops_oee` Python scaffolding is decommissioned.
