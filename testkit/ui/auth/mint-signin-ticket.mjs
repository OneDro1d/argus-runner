// Programmatic sign-in TICKET for a cookieless DEV Clerk instance (no browser sign-in):
// the Admin API mints a one-time sign-in token for CLERK_USER_ID, which window.Clerk can
// activate into a real browser session via signIn.create({strategy:'ticket'}). This is the
// dev-instance-correct path (the raw session token from /sessions/{id}/tokens authenticates
// API calls but not a cookieless-dev browser). Requires CLERK_SECRET_KEY + CLERK_USER_ID.
// Run from testkit/ui/:  node auth/mint-signin-ticket.mjs
import { writeFileSync, mkdirSync, existsSync, readFileSync } from 'node:fs';

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
const USER_ID = process.env.CLERK_USER_ID;
if (!SECRET || !USER_ID) {
  console.error('Set CLERK_SECRET_KEY + CLERK_USER_ID (in .env.test).');
  process.exit(2);
}
const r = await fetch('https://api.clerk.com/v1/sign_in_tokens', {
  method: 'POST',
  headers: { Authorization: `Bearer ${SECRET}`, 'Content-Type': 'application/json' },
  body: JSON.stringify({ user_id: USER_ID }),
});
if (!r.ok) { console.error(`sign_in_tokens failed: ${r.status} ${await r.text()}`); process.exit(1); }
const j = await r.json();
if (!existsSync('playwright/.auth')) mkdirSync('playwright/.auth', { recursive: true });
writeFileSync('playwright/.auth/clerk-ticket.json', JSON.stringify({ ticket: j.token, status: j.status }, null, 2));
console.log(`Minted a Clerk sign-in ticket (status=${j.status}) -> playwright/.auth/clerk-ticket.json`);
