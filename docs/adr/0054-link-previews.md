# 0054. Shared console links show a preview card

Date: 2026-10-31

## Context

A console link pasted into Telegram, Slack or another chat showed as a
bare address: the pages had no Open Graph tags. Chat apps build their
card from a page's `og:title`, `og:description` and `og:image`. They need
absolute addresses, and their fetchers are signed in to nothing.

## Decision

- **Every page carries the tags.** The title is the page's own, with its
  project (`go-test · pipeline #3010474816`, `Pipeline #…: <commit
  title>`). The sentence is the one the page leads with: a build's
  verdict as plain text with its duration, a pipeline's sentence, the
  insights' figures. Pages without a sentence of their own say what the
  console shows of the project. The image is one static card
  (`static/og.png`, 1200×630, source `scripts/og-image.html`), not one
  drawn per page: no image library or fonts in the binary.
- **Absolute addresses** come from where browsers reach the console. The
  chart passes `signIn.url`, else `global.console.url`, to the web as
  `CI_ACCELERATOR_PUBLIC_URL` (an environment variable, so an older image
  still starts). Without it the request's host is used, with the scheme a
  proxy says it served. A host that is not a host leaves the addresses
  out.
- **Sign-in: no project in any card.** A fetcher is signed out, so it gets
  the sign-in page's card. With one GitLab the sign-in page sends a
  browser straight on to GitLab. A known preview fetcher (TelegramBot,
  Slackbot, Twitterbot and others, by user agent) stays on the page, so
  the card is the console's and not GitLab's sign-in. A forged user agent
  only gets the page a browser gets.

## Consequences

- Without sign-in, a card shows what the page shows to anyone who can
  reach it: a verdict names steps and files. That is the console's model
  without sign-in, and the card adds nothing to it.
- Chat apps cache a card. Telegram refreshes one through @WebpageBot.
- The web Deployment gets one environment variable. The web restarts on
  upgrade anyway, for its image.
