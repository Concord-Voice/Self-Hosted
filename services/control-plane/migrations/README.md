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

## Existing Migrations

### 000001_create_users
Creates the core user tables:
- `users` - User accounts with authentication
- `user_keys` - End-to-end encryption key storage

### 000002_create_auth_tables
Creates authentication/session tables:
- `refresh_tokens` - JWT refresh token management

### 000003_create_servers_and_channels
Creates core application tables for Phase 1B:
- `servers` - community-style servers/communities
- `channels` - Voice/text channels within servers
- `server_members` - Server membership and roles

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
