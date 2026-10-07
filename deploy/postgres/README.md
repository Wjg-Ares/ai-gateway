# PostgreSQL deployment files

`migrations/001_initial_schema.sql` creates the first AI Gateway schema. It is
safe to run again: existing tables, indexes, triggers, and the migration marker
are left in place.

Run it on the server after the `ai_gateway` database has been created:

```bash
sudo -u postgres psql -v ON_ERROR_STOP=1 -d ai_gateway \
  -f /path/to/001_initial_schema.sql
```

The migration creates tables only. It does not insert provider keys, gateway
API keys, users, or model rows. When the gateway runs with `AI_GATEWAY_DATABASE_URL`,
the built-in login page is available at `/admin/` (through the existing Xshell
forwarding). Set `AI_GATEWAY_ADMIN_PASSWORD` for the `admin` account; during
the transition, the old `AI_GATEWAY_ADMIN_KEY` value can initialize that
password. Set `AI_GATEWAY_KEY_ENCRYPTION_SECRET` before adding provider keys;
the page encrypts those keys before writing them to `model_keys` and never
displays them again. Re-run this migration after upgrading so existing
databases receive `users.password_hash`.
