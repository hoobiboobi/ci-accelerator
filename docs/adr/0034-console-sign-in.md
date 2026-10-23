# 0034. Console: sign-in through GitLab, inside the console

Date: 2026-10-22

## Context

The console showed every project to whoever reached it, so it had to stay inside the cluster, and it read GitLab with one shared token. People should see a project's builds only when GitLab lets them read that project's jobs, and GitLab should be read with each person's own token. The console must never be reachable from GitLab (no webhooks), must serve several GitLab instances at once (gitlab.com and a company's own), and must not add components the user did not ask for.

Options: oauth2-proxy in front of the console, Dex, or sign-in in the console itself. oauth2-proxy (v7.15.5) serves one GitLab per proxy, refreshes tokens without a lock while GitLab revokes a refresh token on use (so people are signed out every two hours unless Redis is added), checks project membership only at sign-in, does not revoke the token at sign-out, and passes the token in a header any pod allowed by the network policy could forge unless it runs as a sidecar, which its chart does not do. Dex keeps GitLab's token to itself, so it cannot read GitLab as the person.

## Decision

- **Sign-in in the console**, with `golang.org/x/oauth2`: authorization code with PKCE (S256) and state, a confidential OAuth application per GitLab, scopes `openid read_api` (nothing narrower reads jobs). Identity from `GET /api/v4/user`; no OpenID library. The browser carries the redirects; GitLab never calls the console.
- **One callback path per GitLab**, and a sign-in is bound to the GitLab it started with: GitLab's answer does not name its issuer.
- **Sessions in the store**: the cookie (`__Host-`, Secure, HttpOnly, SameSite=Lax) holds 32 random bytes, the store their hash; the person's tokens are sealed (AES-GCM) with a key from a Secret the store does not hold, bound to their session. A session ends after 7 days unused, when GitLab refuses its grant, or at sign-out, which revokes its token at GitLab. Idle sessions are deleted without a revoke, so signing in again needs no new consent.
- **One token renewal per session at a time**, stored before use: GitLab revokes the old access and refresh token at once (checked on gitlab.com). A request refused with the old token gets the new one.
- **What a person sees**: projects whose jobs GitLab lets their token read, asked once per project and kept 10 minutes (a no for 90 seconds); no answer from GitLab is a no. Builds of no known project, other GitLabs' projects and the JSON API: admins only, GitLab usernames in the configuration. Pages of other projects are "not found"; live updates name only readable pipelines.
- **Off by default.** Without it nothing changes. With it, the installation's GitLab token is refused: GitLab is read with each person's token.

## Consequences

- The console can be exposed through an Ingress with TLS; the chart does not create one.
- A person removed from a project still sees it for up to 10 minutes.
- While the console holds any token of a person (another browser), GitLab skips its consent screen; signing out ends only that session's token. Revoking the application in GitLab withdraws all of them.
- The console reads GitLab only while a person's page asks: with sign-in, a pipeline nobody looks at is not read.
- Changing the session key signs everyone out.
