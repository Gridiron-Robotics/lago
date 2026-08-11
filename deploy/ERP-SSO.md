# Lago behind the ERP single login (Keycloak `erp` realm)

Scope: **Lago is owned by this estate for usage-metering only.** This document
describes how Lago joins the Gridiron single-login model where the *only* visible
login is `gridiron-dashboard`. Every other surface — including Lago's admin
dashboard — receives identity transparently: no second login page, no
perceptible redirect round-trip when entering the module.

This repo is the **deploy repo**. The Rails API (`api/`) and React front
(`front/`) are **empty git submodules** (`getlago/lago-api`, `getlago/lago-front`)
and are not checked out here, so the application-level auth changes cannot be
made in this repo. This file is the exact, file-pointed recipe to apply **in the
fork** once those submodules are checked out, plus the deploy plumbing that is
already wired here.

See `../docs` in `erp_django_middleware` (`AUTH_ARCHITECTURE.md`,
`MODULE_SSO_MATRIX.md`) for the estate-wide model. In the SSO matrix Lago is the
`billing.` host, mechanism **JWT**.

## Status (2026-08) — deploy plane wired; fork app-code NOT started

This is a live tracking header so the recipe below does not read as silently
under way when it is not.

- **Deploy plane (this repo): DONE.** The strict gate, realm coordinates, and
  `LAGO_DISABLE_SIGNUP` are threaded through compose + Kamal, default OFF (see
  "What is already wired" below). Realm is `erp`; hosts are
  `keycloak.gridironrobotics.com/realms/erp` and `dashboard.gridironrobotics.com`
  throughout — no stale `gridiron-realm`/`app.` reference remains to repoint.
- **Fork app-code (`getlago/lago-api`, `getlago/lago-front`): NOT STARTED.**
  No fork repo exists yet and nothing is merged. It is **blocked on a decision
  that cannot be taken from this repo**: who owns/hosts the fork, which upstream
  tag it forks from, and who rebases it on each Lago release (the two compose
  files here pin different upstream versions — `v1.27.1` prod vs `v1.48.1` root —
  which is exactly the drift the fork owner must resolve). That JWT-verification
  work is tracked in the estate cross-repo backlog against `getlago/lago-api`
  and `getlago/lago-front`; the file-pointed recipe below is the spec for it.
- **Consequence:** with `LAGO_ERP_SSO_STRICT` unset/false (the shipped default)
  behaviour is byte-for-byte upstream Lago, so nothing here is blocking a
  deploy. Strict mode does nothing enforceable until the fork lands.

---

## What is already wired in this repo (deploy plane)

The SSO gateway host block already exists in the middleware Caddyfile
(`foundation/sso/Caddyfile`):

```
billing.gridironrobotics.com {
    import sso_protect
    reverse_proxy {$LAGO_UPSTREAM:lago-api:3000}
}
```

`import sso_protect` runs `forward_auth` against oauth2-proxy and, on success,
copies these onto the upstream request:

- `Authorization: Bearer <Keycloak access token>` (RS256, realm `erp`)
- `X-Auth-Request-Email`, `X-Auth-Request-User`, `X-Auth-Request-Access-Token`

So the Lago API is reached with a **valid Keycloak Bearer token already
attached** — zero extra round trip. Lago only has to *validate* it.

This repo threads the strict gate + realm coordinates to the API/front via env,
**default OFF** so the vanilla stack is byte-for-byte unchanged:

- `deploy/docker-compose.production.yml` — `x-backend-environment` and
  `x-frontend-environment` anchors now carry `LAGO_ERP_SSO_STRICT`
  (default `false`), `KEYCLOAK_ISSUER`, `KEYCLOAK_JWKS_URL`, `KEYCLOAK_AUDIENCE`,
  `ERP_DASHBOARD_URL`. Unset → empty/false → upstream behavior.
- `config/deploy.yml` (Kamal 2.11.0) — same vars in `env.clear`, with
  `LAGO_ERP_SSO_STRICT: "false"` and the realm URLs pre-filled (non-secret).
- `deploy/.env.production.example` / `deploy/.env.erp-sso.example` — the opt-in
  block operators uncomment to turn strict mode on.

To turn it on at deploy time (once the fork below is in place):

```bash
# docker-compose path
cp deploy/.env.erp-sso.example deploy/.env.erp-sso
# edit: LAGO_ERP_SSO_STRICT=true, then include it:
docker compose --env-file deploy/.env.production --env-file deploy/.env.erp-sso \
  -f deploy/docker-compose.production.yml up -d

# Kamal path: flip LAGO_ERP_SSO_STRICT to "true" in config/deploy.yml env.clear
bundle exec kamal deploy
```

Nothing above changes default behavior: with `LAGO_ERP_SSO_STRICT=false` the
fork must fall through to stock Lago auth.

---

## What to change in the `lago-api` fork (`getlago/lago-api`)

Goal: when `LAGO_ERP_SSO_STRICT=true`, **trust and validate** the injected
Keycloak Bearer JWT on every GraphQL/REST request, auto-provision the Lago
`User`/`Membership` from the token claims, and reject the local
email+password + Google OAuth login mutations. When the flag is false, behave
exactly like upstream.

Concrete pointers (verify exact paths against the checked-out submodule — Lago
moves files between minor releases):

1. **Config surface.** Add a strict-mode config read in
   `config/application.rb` (or a new `config/initializers/erp_sso.rb`):

   ```ruby
   config.x.erp_sso.strict   = ENV["LAGO_ERP_SSO_STRICT"] == "true"
   config.x.erp_sso.issuer   = ENV["KEYCLOAK_ISSUER"]
   config.x.erp_sso.jwks_url = ENV["KEYCLOAK_JWKS_URL"]
   config.x.erp_sso.audience = ENV["KEYCLOAK_AUDIENCE"]
   ```

2. **JWT validation (zero round trip).** Lago already carries `jwt` as a gem.
   Add a `Auth::Erp::KeycloakVerifier` service (mirror the existing
   `app/services/auth/**` layout) that:
   - fetches and caches the realm JWKS from `KEYCLOAK_JWKS_URL` (cache the keys
     in Rails cache / Redis, refresh on `kid` miss — no network call per
     request);
   - `JWT.decode(token, nil, true, algorithms: ["RS256"], jwks:, iss:, verify_iss: true, aud:, verify_aud: KEYCLOAK_AUDIENCE.present?)`;
   - returns the verified claims (`sub`, `email`, realm/`resource_access` roles).

3. **Request authentication hook.** Lago authenticates GraphQL via
   `ApplicationController#current_user` / the GraphQL context builder
   (`app/controllers/api/base_controller.rb` and
   `app/controllers/graphql_controller.rb`, and the token decode in
   `app/services/auth/**` used by `ApplicationController`). When
   `config.x.erp_sso.strict` is true:
   - read the token from `Authorization: Bearer` (already present from
     oauth2-proxy);
   - verify with the service in step 2;
   - `find_or_create` the `User` by `email` claim and attach it to the correct
     `Organization`/`Membership` (auto-provision; map realm roles → Lago
     membership role, defaulting to a read-mostly role for metering);
   - set `context[:current_user]` / `current_organization` from that.
   Fall back to the stock decoder when strict is false.

4. **Disable native login.** In strict mode, make the local login + Google OAuth
   GraphQL mutations refuse:
   - `app/graphql/mutations/auth/login_user.rb`
   - `app/graphql/mutations/auth/google/*` (Google OAuth login/register)
   - `app/graphql/mutations/users/register_user.rb`
   Return a top-level error (`"local login disabled: sign in at the ERP
   dashboard"`) instead of issuing a Lago-native JWT. Keep the mutations present
   (schema stable) but guard the resolver body on `config.x.erp_sso.strict`.

5. **Tests.** Add a request spec proving: (a) a valid Keycloak RS256 token →
   authenticated + user auto-provisioned; (b) a token with a bad signature/`iss`
   → 401; (c) `LoginUser` mutation returns the disabled error under strict; (d)
   strict=false path is unchanged.

`LAGO_DISABLE_SIGNUP=true` (already set in both compose and Kamal env) closes
public self-signup as a belt-and-suspenders measure independent of the flag.

---

## What to change in the `lago-front` fork (`getlago/lago-front`)

Goal: under strict mode the SPA renders **no login form**; it reads the bearer
from the ERP shell (the token oauth2-proxy already established for the browser
session) and, if absent, bounces to `ERP_DASHBOARD_URL`.

1. **Build-time flag.** `lago-front` injects config via `API_URL` at container
   start (`.env` templating in the front image entrypoint). Add
   `APP_ENV`-adjacent reads for `LAGO_ERP_SSO_STRICT` and `ERP_DASHBOARD_URL`
   in `src/core/apolloClient/` config and the app bootstrap
   (`src/index.tsx` / `src/App.tsx`).

2. **Token source.** Lago stores its JWT in `localStorage` under the auth token
   key read by `src/core/apolloClient/` (the `authLink` sets
   `Authorization: Bearer`). In strict mode, seed that from the ERP shell:
   read `X-Auth-Request-Access-Token` exposed by the shell (or the shell's
   `postMessage`/shared cookie), instead of from the `LoginPage` flow.

3. **Kill the login UI.** Guard the auth routes in `src/App.tsx` /
   `src/pages/auth/*` (`Login.tsx`, `SignUp.tsx`, `GoogleAuth*`): when strict,
   do not mount them; unauthenticated → `window.location.replace(ERP_DASHBOARD_URL)`.

4. **Logout.** Point logout at the ERP dashboard (single logout is the shell's
   job) rather than Lago's local `/login`.

---

## Acceptance (matches `MODULE_SSO_MATRIX.md`)

- Entering `billing.gridironrobotics.com` from an authenticated shell shows the
  Lago dashboard with **no login page and no perceptible redirect**.
- Lago validates the Keycloak Bearer JWT against the realm JWKS (`iss`, `exp`,
  and — when `KEYCLOAK_AUDIENCE` set — `aud`); an exposed port with a forged
  header is rejected.
- Local email/password login and Google OAuth are disabled in strict mode.
- With `LAGO_ERP_SSO_STRICT=false` (the default), the stack is identical to
  upstream Lago.
