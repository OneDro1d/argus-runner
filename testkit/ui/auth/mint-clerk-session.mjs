// Programmatic Clerk auth (no browser) for unattended runs (CI / agent sessions).
// Mints a Clerk JWT via the Admin API and writes a synthetic storageState.
//   Requires:  CLERK_SECRET_KEY  +  CLERK_SESSION_ID  +  APP_DOMAIN  (in .env.test / env)
//   Run from testkit/ui/:  node auth/mint-clerk-session.mjs
//
// Why this works where cookie-injection doesn't: Playwright loads the cookies at
// BrowserContext creation time (before the first navigation), so Clerk's SDK sees a
// fresh, legitimate session rather than a mid-flight set that it rejects.
import { writeFileSync, mkdirSync, existsSync, readFileSync } from 'node:fs';

// Load .env.test (gitignored) so the mint runs standalone — same parser as
// playwright.config.ts (split on the first '='; never overrides an already-set env var,
// so a shell export still wins). Run from testkit/ui/.
if (existsSync('.env.test')) {
  for (const line of readFileSync('.env.test', 'utf8').split(/\r?\n/)) {
    const t = line.trim();
    if (!t || t.startsWith('#')) continue;
    const eq = t.indexOf('=');
    if (eq < 0) continue;
    const k = t.slice(0, eq).trim();
    if (!(k in process.env)) process.env[k] = t.slice(eq + 1).trim();
  }
}

const SECRET = process.env.CLERK_SECRET_KEY;
const SESSION_ID = process.env.CLERK_SESSION_ID;
const DOMAIN = process.env.APP_DOMAIN || 'app.example.com';
const CLERK_ENV = process.env.CLERK_ENVIRONMENT_JSON || '{}'; // captured once from a real session
const OUTPUT = 'playwright/.auth/clerk-session.json';

if (!SECRET || !SESSION_ID) {
  console.error('Set CLERK_SECRET_KEY + CLERK_SESSION_ID (find an active session: GET https://api.clerk.com/v1/sessions?user_id=<id>&status=active).');
  process.exit(2);
}

const r = await fetch(`https://api.clerk.com/v1/sessions/${SESSION_ID}/tokens`, {
  method: 'POST',
  headers: { Authorization: `Bearer ${SECRET}`, 'Content-Type': 'application/json' },
});
if (!r.ok) { console.error(`Clerk token mint failed: ${r.status} ${await r.text()}`); process.exit(1); }
const jwt = (await r.json()).jwt;

const now = Math.floor(Date.now() / 1000);
const expires = now + 22 * 86400;
// The app origin (where the SPA actually runs) — the localStorage must be keyed to THIS
// origin, not https://<domain>, or the browser ignores it. secure-cookies only over https.
const ORIGIN = (process.env.APP_URL || `https://${DOMAIN}`).replace(/\/$/, '');
const secure = ORIGIN.startsWith('https://');
const cookie = (name, value) => ({ name, value, domain: DOMAIN, path: '/', expires, httpOnly: false, secure, sameSite: 'Lax' });
const state = {
  cookies: [
    cookie('__session', jwt),
    cookie('__clerk_db_jwt', jwt),
    // __client_uat is Clerk's "user authenticated at" heuristic: the SDK treats __client_uat>0
    // as signed-in (and only then consumes __session/__clerk_db_jwt). Without it the SDK
    // assumes signed-out and renders the sign-in wall — so a minted session never authenticates.
    cookie('__client_uat', String(now)),
  ],
  origins: [{ origin: ORIGIN, localStorage: [{ name: '__clerk_environment', value: CLERK_ENV }] }],
};
if (!existsSync('playwright/.auth')) mkdirSync('playwright/.auth', { recursive: true });
writeFileSync(OUTPUT, JSON.stringify(state, null, 2));
console.log(`Minted a Clerk session storageState → ${OUTPUT} (JWT ~60s TTL; Clerk auto-refreshes after load).`);
