# Flow — Cross-Document Contract Matrix

The owner document defines the normative contract. References may summarize it but must not redefine it.

| Contract | Owner | Required references |
|---|---|---|
| Authentication and browser sessions | ADR-002; SECURITY §5 | REQUIREMENTS FR-AUTH, API §1/§9, DATABASE_SCHEMA `sessions`, ROADMAP M1 |
| Navidrome administrator credentials and external key custody | ADR-003; SECURITY §4/§10 | ARCHITECTURE §3.1/§8, DATABASE_SCHEMA `credential_key_versions`, NAVIDROME_INTEGRATION §3/§5, DEPLOYMENT §4/§5 |
| Membership provisioning and crash recovery | ADR-004 | REQUIREMENTS FR-MEM-004, DATABASE_SCHEMA `users`, NAVIDROME_INTEGRATION §4, API §7, ROADMAP M1 |
| Streaming gateway | ADR-001 | REQUIREMENTS FR-STRM, ARCHITECTURE §5.2, API §2.1, PLAYBACK_SYSTEM §6/§10, SECURITY §4 |
| Upload lifecycle and recovery | NAVIDROME_INTEGRATION §6 | REQUIREMENTS FR-ADM-005/006, PRODUCT_SPEC §5.3, DATABASE_SCHEMA `upload_operations`, SECURITY §7, DEPLOYMENT topology, ROADMAP M5 |
| Individual playback CAS, persistence and browser transport | PLAYBACK_SYSTEM §2; ADR-007 | REQUIREMENTS FR-IND-001…006, API §4, DATABASE_SCHEMA `session_state`, ARCHITECTURE §6, ROADMAP M3 |
| Sync state, `version`, and `event_seq` | ADR-006 | REQUIREMENTS FR-SYN-007, DATABASE_SCHEMA `shared_sessions`/`sync_events`, API §5/§9, PLAYBACK_SYSTEM §3, ROADMAP M4A/M4B |
| DJ lease, disconnect, and reconnect | ADR-005 | REQUIREMENTS FR-SYN-008, DATABASE_SCHEMA `shared_sessions`, API §5, PLAYBACK_SYSTEM §3.5/§4, SECURITY acceptance tests |
| M4A public mode transition and internal lease boundary | M4A_READINESS | API §5/§6, ARCHITECTURE mode sequence, PLAYBACK_SYSTEM T1/T2, `internal/sync`, `internal/api/sync.go` |
| M4A execution evidence and remaining database gate | M4A_REPORT | ROADMAP M4A, DEVELOPMENT_GUIDE testing, `internal/sync/*_test.go` |
| Backend restart | ADR-005 | PRODUCT_SPEC §3.3, REQUIREMENTS FR-SYN-011/NFR-REL-004, PLAYBACK_SYSTEM §3.6, DEVELOPMENT_GUIDE §5 |
| Configuration, migrations, and deployment | DEPLOYMENT; DEVELOPMENT_GUIDE | ARCHITECTURE §8, REQUIREMENTS NFR-MAINT, SECURITY §8-§10, ROADMAP M0/M6 |
| V1/V1.x feature scope | PRODUCT_SPEC §10; ROADMAP | README scope, REQUIREMENTS priorities, API route presence/absence, ARCHITECTURE package plan |

## Consistency rules

1. Requirement IDs remain stable even when text is clarified or priority changes.
2. An API route cannot claim a capability earlier than its roadmap milestone.
3. Source-verified and runtime-verified Navidrome claims are labeled separately.
4. Accepted ADRs override summaries in non-owner documents; changing an accepted decision requires a superseding ADR.
