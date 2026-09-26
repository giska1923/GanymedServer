# GanymedServer Documentation

The backend for GanymedEngine. Not to be confused with `GanymedDedicated`, the engine app that
runs one match headless and that this backend allocates. See [AGENTS.md](../AGENTS.md).

## Layout

| Folder | Holds | Tense |
|---|---|---|
| [`ToDo/`](ToDo/README.md) | Design docs, phase plans, known bugs, deferred follow-ups: anything not done yet | future |
| `backend/` | What the code does now, one file per module. Created as modules land | present |
| `history/` | Completed phase records, with rationale and verification evidence. Immutable | past |
| [`api/`](api/README.md) | The contract with the engine and game servers | normative |

## Documents

### Design and plans

| Document | Covers |
|---|---|
| [BACKEND.md](ToDo/BACKEND.md) | The design: architecture, the module rule, storage, contract, testing, dependencies, and phases B1–B5 |

### Modules

None yet. Each module gets `backend/<module>.md` in the change that creates it, indexed here.

### Contract

See [api/README.md](api/README.md).

## The engine side

The engine's half of this integration is `docs/ToDo/ONLINE.md` in the
[GanymedEngine](https://github.com/giska1923/GanymedEngine) repo (phases O0–O5). It links to
this repo's `docs/api/` at a tagged version.
