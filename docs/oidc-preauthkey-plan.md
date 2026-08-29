# Plan: Add OIDC pre-auth key support to `mint-authkey`

**Date:** 2026-07-13
**Author:** Claude
**Status:** Draft — awaiting review

---

## 1 Problem statement

`bin/mint-authkey` mints Headscale pre-auth keys by resolving a user **by name**. It calls

```
POST /api/v1/user { "name": "<HEADSCALE_USER>" }
→ 409 Conflict   → GET /api/v1/user?name=<USER>  → read .id
→ otherwise      → read .user.id
```

This works for locally-created users. With **OIDC enabled**, a user authenticates via their IdP and receives a record with:

| Field | Example value |
|---|---|
| `Name` | `Chris Patton` (display name) |
| `Email` | `user@example.com` |
| `Provider` | `oidc` |
| `ProviderIdentifier` | `https://sso.example.com/user_01` |

The admin sets `HEADSCALE_USER`
in `.env` to something the tool recognises, but the IdP-assigned display name is different.
The tool finds no exact name match and **creates a brand-new local user** with a fresh
`provider_identifier = NULL`. That local user is **not** the real OIDC identity — any auth key
minted for it goes to the wrong place.

Passing what looks like the email address (a non-matching name set as the user variable)
has the same symptom: it creates yet another local user.

---

## 2 Headscale internals (relevant facts)

### 2.1 User identity model

Headscale distinguishes three registration methods (`hscontrol/util/const.go`):

| Constant | Meaning |
|---|---|
| `RegisterMethodAuthKey = "authkey"` | Pre-auth key registration (local) |
| `RegisterMethodOIDC = "oidc"` | OIDC login |
| `RegisterMethodCLI = "cli"` | Manual creation |

The `users` table carries two critical columns:

```sql
CREATE TABLE users(
    id integer PRIMARY KEY AUTOINCREMENT,
    name text,                 -- display/username
    email text,                -- primary email (OIDC principal)
    provider_identifier text,  -- issuer/sub (unique per OIDC user)
    provider text              -- 'oidc', '', or other
);

-- Constraints:
-- idx_provider_identifier    : UNIQUE WHERE provider_identifier IS NOT NULL
-- idx_name_no_provider_id    : UNIQUE WHERE provider_identifier IS NULL
-- idx_name_provider_id       : UNIQUE composite of (name, provider_identifier)
```

A user whose `provider_identifier` is non-NULL is an **OIDC user**; one with `NULL` is **local**. They are mutually exclusive — you cannot promote a local user to OIDC or vice versa.

### 2.2 How Headscale locates OIDC users internally

During OIDC callback flow (`hscontrol/oidc.go:createOrUpdateUserFromClaim`), the server:

1. Builds an **OIDC Identifier** from `issuer + "/" + subject` claims.
2. Calls `GetUserByOIDCIdentifier(identifier)` — a DB query against `provider_identifier`.
3. If found → updates the existing record.
4. If not found → creates a new user from claim data.

The OIDC Identifier is **opaque to external tools** — it depends on the IdP's issuer URL and the user's subject claim, neither of which an admin typically knows.

### 2.3 Available API lookup paths

| API | Lookup method | OIDC capable? |
|---|---|---|
| `GET /api/v1/user?name=X` | Exact name match (all users) | Partially — finds OIDC users by name too, but ambiguous if name collides |
| `GET /api/v1/user?email=X` | Exact email match (all users) | **Yes — reliable single match** unless multiple OIDC users share the email |
| gRPC `GetUserByOIDCIdentifier(id)` | ProviderIdentifier exact match | Fully, but requires gRPC client |

Headscale stores **no index** on the `email` column (SQLite lacks expression indexes), but for typical deployments (< 1000 users) the scan is fast.

### 2.4 Can nodes register via OIDC using an auth key?

**Yes.** A node registered with a pre-auth key under an OIDC user still resolves to the correct user. Subsequent logins verify ownership through OIDC (SSH check-mode confirms the connecting user owns the node). There is no protocol-level restriction preventing auth-key-based registration under OIDC users.

Reference: `hscontrol/oidc.go:handleRegistration` accepts a `types.User*` regardless of whether it originated from OIDC or local creation — the OIDC verification happens later during node login, not registration.

---

## 3 Proposed solution

Modify the `ensure_user()` function in `bin/mint-authkey` to use a **multi-strategy lookup**:

```
try: find user by name (exact match)
  → return user ID (works for both local and OIDC)
except (ambiguous name match → N > 1):
  try: find user by email (exact match)
    → if exactly one OIDC user found → return its ID
    → else → fail with clear diagnostic
else:
  no user found by name either → create user (existing behaviour)
```

This preserves backward compatibility (local users still work unchanged) while handling the OIDC case correctly.

---

## 4 Detailed design

### 4.1 New CLI flag: `--email`

Add an optional `--email <value>` argument:

```
bin/mint-authkey --email chris@chiiiirs.com
```

When provided, `ensure_user()` uses the multi-strategy lookup described above. Without the flag, the tool behaves exactly as it does today (lookup-and-create-by-name).

**Rationale:** Keeping it opt-in avoids changing behavior for existing users who rely on name-based lookup, while giving OIDC users a path forward.

### 4.2 Multi-strategy lookup algorithm

```python
def ensure_user(base_url, key, name, email=None):
    # Step 1: Try exact name match (local + OIDC alike)
    result = api(base_url, "POST", "user", key, {"name": name})
    if "user" in result:          # immediately created or returned by HEADscale
        return result["user"]["id"]

    # 409 → name already exists. List to get its ID.
    users = api(base_url, "GET", "user", key, params={"name": name})
    matches = [u for u in users.get("users", []) if u["name"] == name]

    if len(matches) == 1:
        return matches[0]["id"]     # unambiguous → done

    # Step 2: Name collision → try email fallback
    if email:
        by_email = api(base_url, "GET", "user", key, params={"email": email})
        oidc_matches = [
            u for u in by_email.get("users", [])
            if u.get("provider") == "oidc"
        ]

        if len(oidc_matches) == 1:
            return oidc_matches[0]["id"]

        msg = f"user '{name}' matched {len(matches)} names"
        if email:
            msg += f"; email '{email}' matched {len(oidc_matches)} OIDC users"
        die(msg)

    die(f"user '{name}' found {len(matches)} times; specify --email to disambiguate")
```

### 4.3 Header output format

Print the resolved user identity to stderr so the caller can confirm correctness:

```
Resolved OIDC user "Chris Patton" <chris@chiiiirs.com> (id=5, provider=oidc)
Minted pre-auth key for user "Chris Patton" (expires in 90d).
<authkey>
```

For local users, print identically but with `provider=` omitted or shown as empty.

### 4.4 `.env` documentation update

Update `.env.example` with comments:

```sh
# Headscale user the node registers under.
# For OIDC users, also provide --email so the tool can resolve the correct
# user (OIDC users are identified by their IdP email, not by display name).
HEADSCALE_USER=user
# Optional: full email of the target OIDC user, used for disambiguation
# HEADSCALE_USER_EMAIL=user@example.com
```

Also consider supporting `HEADSCALE_USER_EMAIL` from env (mirroring how `HEADSCALE_ADMIN_KEY_SECRET` and similar options are configured), so the `--email` flag becomes a convenience shortcut and the `.env` approach works in automation pipelines.

### 4.5 Environment variable: `HEADSCALE_USER_EMAIL`

Support reading the email from `.env` as `HEADSCALE_USER_EMAIL`, so CI/automation can operate without CLI args:

```sh
HEADSCALE_USER=user
HEADSCALE_USER_EMAIL=user@example.com
./bin/mint-authkey                    # reads email from .env automatically
```

---

## 5 Edge cases & error messages

| Scenario | Behaviour |
|---|---|
| Name exists as a **local** user only | Creates/returns local user as before. Email filter ignored (non-OIDC). |
| Name matches **multiple OIDC** users from the same provider | Fails: `"user 'bob' matched 2 names; specify --email to disambiguate"` |
| `--email` given but **no** OIDC user has that email | Fails: `"email 'wrong@example.com' matched 0 OIDC users"` |
| `--email` matches **multiple** OIDC users (same email shared) | Fails: `"email 'shared@example.com' matched 3 OIDC users — cannot disambiguate"` |
| Both name and email produce a match but they point to **different** users | Warns (or errors). Prefer the email match since it targets the OIDC identity specifically. |

---

## 6 Testing plan

### 6.1 Unit tests (Go tests in `mint_authkey_test.go`)

No Go unit test applies directly to Python logic, but the existing
`TestWriteEnvUsesOwnerOnlyPermissions` validates file-permission behavior. Add:

- **Mock-based test** for the multi-strategy lookup: mock the four API calls
  (`POST /user`, `GET /user?name=…`, `GET /user?email=…`, `POST /user` for creation)
  and assert correct branch taken for each scenario.

Given the tool is pure Python with subprocess deps (`aws` CLI), use `pytest` with
`unittest.mock.patch` or add a lightweight shell-test wrapper.

### 6.2 Integration test (manual / CI-ready)

1. Configure a test Headscale instance with OIDC enabled.
2. Register a known OIDC user via the UI (e.g., `test-user@test.com`).
3. Run:

   ```sh
   # Set HEADSCALE_USER and HEADSCALE_USER_EMAIL in .env before running.
   ./bin/mint-authkey
   ```

4. Verify the returned key's user field in Headplane shows the **original OIDC user**,
   not a newly created local user.

---

## 7 Implementation checklist

- [ ] Add `--email` CLI argument to `argparse` in `main()`.
- [ ] Add `HEADSCALE_USER_EMAIL` config support (from dotenv / env).
- [ ] Rewrite `ensure_user()` with the multi-strategy lookup.
- [ ] Print resolved user identity (name, email, provider) to stderr before minting.
- [ ] Update `--help` text describing OIDC usage.
- [ ] Update `.env.example` with commented-out `HEADSCALE_USER_EMAIL` example.
- [ ] Add tests for the lookup branches (unit test using mocked HTTP responses).
- [ ] Update `docs/usage.md` with the OIDC workflow section.
- [ ] Run existing `make test` / manual smoke test.

---

## 8 Risks & mitigations

| Risk | Impact | Mitigation |
|---|---|---|
| Heascale `email` column unindexed (SQLite) | Slow list queries on large tenants (> 10k users) | Not expected in practice; tailscale tailnets rarely exceed ~10k. If it becomes an issue, upstream could add a partial index. |
| Admin specifies wrong email | Creates a *new* local user with that name (old bug surface) | The multi-strategy lookup prevents creation until a valid OIDC match is found. No stale local user gets created. |
| OIDC users sharing the same email | Disambiguation fails | Explicit error message directs admin to use `HEADSCALE_USER` matching a unique name, or contact the IdP admin. |
| Non-OIDC admins accidentally use `--email` | Confusing error if user has no OIDC provider | The email filter excludes non-OIDC users from consideration; falls through to name lookup gracefully. |

---

## 9 Alternatives considered (and rejected)

### 9.1 gRPC lookup by OIDC identifier

Use the gRPC API to call `GetUserByOIDCIdentifier`. Requires knowing the exact `(iss, sub)` pair upfront — impractical for most admins. Also adds a protobuf dependency (`grpcio-tools`) to the Python tool.

**Rejected:** Too much setup complexity for marginal benefit.

### 9.2 Auto-resolve by email without `--email`

Make email lookup the default whenever name lookup is ambiguous. Changes existing behavior for name-collision edge cases.

**Rejected:** Breaks backward compatibility for anyone relying on the current name-first semantics.

### 9.3 Headscale-side fix: auto-match email during user creation

Patch Headscale to check existing OIDC users' emails before creating a new user. Would solve the root cause but requires modifying upstream and waiting for release.

**Rejected:** Out of scope for this project; add our own workaround in the tool while upstream matures.

### 9.4 Admin must always use the user's **display name**

Instruct admins to look up the OIDC user's display name in the Headscale UI and use that.

**Rejected:** Error-prone — display names may differ from usernames, contain spaces, or change over time. The email is stable and well-known.

---

## 10 Appendix: Relevant source locations

| File | Relevance |
|---|---|
| `bin/mint-authkey` | Tool to be modified |
| `hscontrol/api/v1/users.go` | `createUser`, `listUsersFiltered` — v1 REST endpoints |
| `hscontrol/api/v1/types.go` | `User` struct — includes `provider`, `providerId` fields |
| `hscontrol/db/users.go` | `GetUserByName`, `GetUserByOIDCIdentifier`, `CreateUser` — DB layer |
| `hscontrol/types/users.go` | `User` struct definition, `Username()` resolution order |
| `hscontrol/oidc.go` | `createOrUpdateUserFromClaim` — OIDC user matching logic |
| `hscontrol/util/const.go` | `RegisterMethodOIDC`, `RegisterMethodAuthKey` constants |
| `hscontrol/db/schema.sql` | Table layout, indexes including `idx_provider_identifier` |
| `.env.example` | Config template to update |
| `docs/usage.md` | Documentation to update |
| `mint_authkey_test.go` | Existing test — add new lookup tests here |
