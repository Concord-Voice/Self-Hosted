# Database Migrations

This directory contains versioned database migrations for the Concord Control Plane service using [golang-migrate](https://github.com/golang-migrate/migrate).

## Migration File Naming

Migrations are named with the following pattern:
```
{version}_{description}.{direction}.sql
```

Examples:
- `000001_create_users.up.sql` - Creates users table
- `000001_create_users.down.sql` - Drops users table (rollback)
- `000002_create_auth_tables.up.sql` - Creates auth-related tables
- `000002_create_auth_tables.down.sql` - Drops auth tables (rollback)

## Creating New Migrations

### Using Make (Recommended)

```bash
cd services/control-plane
make migrate-create NAME=add_user_status
```

This will create two files:
- `migrations/000004_add_user_status.up.sql` - Forward migration
- `migrations/000004_add_user_status.down.sql` - Rollback migration

### Manual Creation

You can also manually create migration files following the naming pattern above.

## Running Migrations

### Apply All Pending Migrations

```bash
make migrate-up
```

This is automatically run when the control-plane server starts.

### Rollback Last Migration

```bash
make migrate-down
```

### Check Current Version

```bash
make migrate-version
```

## Migration Best Practices

1. **Always create both UP and DOWN migrations** - Every migration must be reversible
2. **Test rollbacks** - Ensure your down migration properly reverses the up migration
3. **One logical change per migration** - Don't mix unrelated schema changes
4. **Use transactions implicitly** - Each migration file runs in its own transaction
5. **Avoid data migrations in schema migrations** - Consider separate data migration scripts
6. **Never modify committed migrations** - Create new migrations to fix issues

## Migration Structure

### Up Migration (`*.up.sql`)
```sql
-- Migration: add_user_status (up)
-- Purpose: Add status field to users table

ALTER TABLE users ADD COLUMN status VARCHAR(20) DEFAULT 'active';
CREATE INDEX idx_users_status ON users(status);
```

### Down Migration (`*.down.sql`)
```sql
-- Migration: add_user_status (down)
-- Purpose: Remove status field from users table

DROP INDEX IF EXISTS idx_users_status;
ALTER TABLE users DROP COLUMN IF EXISTS status;
```

## Existing Migrations (28 total: 000001–000028)

### Phase 1A — Authentication & E2EE
| # | Name | Tables/Changes |
|---|------|---------------|
| 000001 | create_users | `users`, `user_keys` |
| 000002 | create_auth_tables | `refresh_tokens` |
| 000004 | public_keys | `public_keys` |
| 000011 | session_revoked_at | Adds `revoked_at` to sessions |
| 000012 | remember_me | Remember-me persistent sessions |

### Phase 1B — Channels & Text Chat
| # | Name | Tables/Changes |
|---|------|---------------|
| 000003 | create_servers_and_channels | `servers`, `channels`, `server_members` |
| 000005 | server_icon_url | Adds `icon_url` to servers |
| 000006 | messages_table | `messages` |
| 000007 | user_profile_fields | Profile fields (display_name, bio, avatar, links) |
| 000008 | channel_fields | Channel emoji, description, position |
| 000009 | server_invites | `server_invites` |
| 000010 | channel_read_states | `channel_read_states` |
| 000013 | channel_encryption | E2EE channel enforcement |
| 000014 | channel_keys | `channel_keys`, `pending_key_requests` |
| 000015 | fix_invite_timestamps | Timestamp corrections |
| 000016 | user_preferences | `user_preferences` |
| 000017 | fix_timestamps | Additional timestamp fixes |
| 000018 | message_composite_index | Performance index on messages |
| 000019 | header_image_url | Header/banner images |

### Phase 1C — Voice, Media & Desktop Hardening
| # | Name | Tables/Changes |
|---|------|---------------|
| 000020 | voice_participants | `voice_participants` |
| 000021 | channel_voice_settings | Voice channel settings |
| 000022 | channel_groups | Channel groups/categories |
| 000023 | voice_text_channels | Voice text chat support |
| 000024 | user_color_scheme | Per-user color schemes |
| 000025 | revocation_mode | Session revocation modes |
| 000026 | dm_system | `dm_conversations`, `dm_participants`, `dm_messages`, `dm_read_states`, `dm_channel_keys`, `dm_pending_key_requests`, `dm_voice_participants` |
| 000027 | friend_codes_and_privacy | `friendships`, `friend_codes`, `privacy_settings` |
| 000028 | machine_id_and_key_version | Machine ID tracking, key versioning |

## Troubleshooting

### Dirty Migration State

If a migration fails mid-execution, the database may be in a "dirty" state:

```bash
# Check if dirty
make migrate-version

# To fix, manually correct the database and force version
# OR rollback to previous version and reapply
```

### Migration Conflicts

If working in a team and migrations conflict:
1. Pull latest changes
2. Rename your migration to have a higher version number
3. Update any references to the migration

## Environment Variables

Migrations use the `DATABASE_URL` environment variable:

```bash
# .env file
DATABASE_URL=postgres://concord:concord_dev_password@localhost:5432/concord?sslmode=disable
```

## CI/CD Integration

In production, migrations should run automatically before deploying new code:

```yaml
# .github/workflows/deploy.yml
- name: Run migrations
  run: |
    cd services/control-plane
    make migrate-up
```
