#!/usr/bin/env python3
"""Disposable SQL smoke for environments that cannot start a PostgreSQL listener.

This is not the concurrent pgx/HTTP integration gate. Use FLOW_TEST_DATABASE_URL
and go test -race for that. No existing database or operator config is touched.
"""

from pathlib import Path
import shutil
import subprocess
import tempfile


def main():
    for executable in ("initdb", "postgres"):
        if not shutil.which(executable):
            raise SystemExit(f"{executable} is required")
    migrations = Path(__file__).resolve().parents[1] / "backend" / "migrations"
    with tempfile.TemporaryDirectory(prefix="flow-m3-schema-") as directory:
        data = Path(directory) / "data"
        subprocess.run(
            ["initdb", "-D", str(data), "--auth=trust", "--no-locale", "-E", "UTF8"],
            capture_output=True, text=True, check=True,
        )

        def sql(statement):
            result = subprocess.run(
                ["postgres", "--single", "-j", "-D", str(data), "postgres"],
                input=statement + "\n\n", capture_output=True, text=True, timeout=30,
            )
            if result.returncode or "ERROR:" in result.stderr or "FATAL:" in result.stderr:
                raise RuntimeError(result.stdout + result.stderr)

        def migration(name):
            sql((migrations / name).read_text())

        migration("0001_m0_foundation.up.sql")
        migration("0002_m1_auth_membership.up.sql")
        sql("""
            INSERT INTO users(username,display_name,password_hash,role,status,provisioning_state)
            VALUES('before-m3','Before M3','test-only','member','active','not_started');
        """)
        migration("0003_m3_individual_playback.up.sql")
        sql("""
            INSERT INTO users(username,display_name,password_hash,role,status,provisioning_state)
            VALUES('after-m3','After M3','test-only','member','active','not_started');
            DO $$
            DECLARE changed integer;
            BEGIN
                IF (SELECT count(*) FROM session_state) <> 2 THEN
                    RAISE EXCEPTION 'backfill/user-insert trigger failed';
                END IF;
                IF EXISTS(SELECT 1 FROM session_state WHERE revision <> 0 OR volume <> 1
                    OR queue <> '[]'::jsonb OR playback_status <> 'paused') THEN
                    RAISE EXCEPTION 'default session failed';
                END IF;
                UPDATE session_state SET volume=0.2, revision=revision+1 WHERE user_id=1 AND revision=0;
                GET DIAGNOSTICS changed = ROW_COUNT;
                IF changed <> 1 THEN RAISE EXCEPTION 'first conditional update failed'; END IF;
                UPDATE session_state SET volume=0.8, revision=revision+1 WHERE user_id=1 AND revision=0;
                GET DIAGNOSTICS changed = ROW_COUNT;
                IF changed <> 0 THEN RAISE EXCEPTION 'stale conditional update wrote state'; END IF;
                IF (SELECT volume FROM session_state WHERE user_id=2) <> 1 THEN
                    RAISE EXCEPTION 'cross-user write';
                END IF;
                BEGIN
                    UPDATE session_state SET volume=-0.1 WHERE user_id=1;
                    RAISE EXCEPTION 'volume constraint missing';
                EXCEPTION WHEN check_violation THEN NULL; END;
                BEGIN
                    UPDATE session_state SET position_ms=-1 WHERE user_id=1;
                    RAISE EXCEPTION 'position constraint missing';
                EXCEPTION WHEN check_violation THEN NULL; END;
                BEGIN
                    UPDATE session_state SET current_media_id='a' WHERE user_id=1;
                    RAISE EXCEPTION 'empty queue constraint missing';
                EXCEPTION WHEN check_violation THEN NULL; END;
                BEGIN
                    INSERT INTO session_state(user_id) VALUES(1);
                    RAISE EXCEPTION 'one-row constraint missing';
                EXCEPTION WHEN unique_violation THEN NULL; END;
                UPDATE session_state SET
                    queue='[{"mediaId":"a","addedAt":"2026-11-06T00:00:00Z"},{"mediaId":"b","addedAt":"2026-11-06T00:00:01Z"}]',
                    queue_index=0,current_media_id='a',position_ms=10000,playback_status='playing',revision=revision+1
                    WHERE user_id=1 AND revision=1;
                BEGIN
                    UPDATE session_state SET queue_index=2 WHERE user_id=1;
                    RAISE EXCEPTION 'queue bounds constraint missing';
                EXCEPTION WHEN check_violation THEN NULL; END;
                BEGIN
                    UPDATE session_state SET current_media_id='b' WHERE user_id=1;
                    RAISE EXCEPTION 'current/index agreement constraint missing';
                EXCEPTION WHEN check_violation THEN NULL; END;
            END $$;
        """)
        # A new PostgreSQL backend process must see the committed checkpoint.
        sql("""
            DO $$ BEGIN
                IF NOT EXISTS(SELECT 1 FROM session_state WHERE user_id=1 AND revision=2
                    AND current_media_id='a' AND position_ms=10000 AND volume=0.2) THEN
                    RAISE EXCEPTION 'state did not survive backend process restart';
                END IF;
            END $$;
        """)
        migration("0003_m3_individual_playback.down.sql")
        sql("""
            INSERT INTO users(username,display_name,password_hash,role,status,provisioning_state)
            VALUES('during-down','During down','test-only','member','active','not_started');
        """)
        migration("0003_m3_individual_playback.up.sql")
        sql("""
            DO $$ BEGIN
                IF (SELECT count(*) FROM session_state) <> 3 THEN
                    RAISE EXCEPTION 'down/reapply lost M1 accounts or failed to backfill';
                END IF;
            END $$;
        """)
        version = subprocess.run(["postgres", "--version"], capture_output=True, text=True, check=True).stdout.strip()
        print(f"{version}: M3 upgrade, backfill, trigger, constraints, conditional writes, restart, down/reapply passed")
        print("Single-user SQL smoke only; concurrent pgx/HTTP integration remains a separate gate.")


if __name__ == "__main__":
    main()
