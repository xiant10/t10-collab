# t10-modkit

Shared Go plumbing for t10's services: live org reads (`people`), random IDs
(`ids`), keyset cursors (`page`), Postgres migrations (`pgdb`), request traces
(`telemetry`), JSON bodies and the error body (`httpjson`), web sign-in
(`websession`), the app frame (`frame`) and docs over MCP (`docsmcp`). Business policy
stays in each consumer. `people.Member` always reads fresh; its separate reports
cache is not suitable for knowledge membership authorization.

Run `make test`, or `make db-up` and `make test-db` for `pgdb`'s tests against Postgres. This repository is not published yet. Consumers carry a
source snapshot in `third_party/t10-modkit` and a local replacement in go.mod,
so builds never need a sibling checkout. Refresh snapshots with
`./scripts/sync-modkit.sh` in the workspace and check parity with
`./scripts/check-modkit.sh`. Replace snapshots with a tagged dependency after
this repository has a remote and release. No remote has been created.

Extraction is incremental. Verifier parity remains governed by auth-server;
route guard composition and scheduler locks remain to be extracted.

Source packages were extracted from t10-collab and retain its Apache 2.0 license; see [LICENSE](LICENSE).
