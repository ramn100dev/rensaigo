# Changelog

## 0.2.1 — 2026-11-23

Fixes reported by the first user migrating from Rensaiō (#1).

**Fixed**
- Linking a source to an imported series no longer queues every older chapter that is not on disk (a series kept as
  volumes plus chapters from 374 queued 348 chapters). What the source has below the highest chapter on disk stays
  *available*; only newer chapters are queued. `/import/{id}/match`, `/import/{id}/auto` and `/series/follow` with
  `add_to` also take `from` to start at a given chapter.
- Adding the first source of an imported series with `/series/follow` + `add_to` left it paused.
- `POST /api/install` enabled all 60 languages of a multi-language extension; it now does the same as the button
  (only your preferred language) and probes the new sources.
- The library scan no longer reads CBZ files while holding the database write lock: on a slow disk the first scan of
  a big library could make other actions fail with `database is locked`.
- API errors that do not come from the extension host are reported as `"kind": "internal"` instead of `"host"`, and
  carry a readable message (the original goes in `detail`).
- Extensions that need a built-in browser (WebView) say so clearly instead of showing a Java stack trace.

## 0.2.0 — 2026-11-23

Robustness release: a full review with many more tests (download engine, extension host supervisor, every page in
both languages, tracking sync, notifications, import, fuzzing) and the bugs they turned up.

**Fixed**
- Requests coming from another website are rejected (CSRF). Without a password, any page you visited could
  otherwise send a form to Rensaigo and, for example, delete a series.
- Deleting a series (or removing a source) while one of its chapters was downloading brought its folder back.
- Two chapters with very long titles could end up with the same file name and overwrite each other.
- If the library disk is not mounted, the scan no longer marks every chapter as missing.
- After a chapter was replaced by a better copy, the browser could keep showing the old pages.
- A failure that was not a host crash (a source returning no pages, a site that does not answer in time) was
  retried every 3 seconds and the chapter ended up failed within seconds; it now backs off as intended.
- A series linked to a source whose extension is not installed (restored from `rensaigo.json`) broke its page.
- "Add from list" with no connected account, and a few untranslated texts (OPDS feed, toasts).
- Background tasks recover from unexpected errors instead of taking the whole process down.

**Added**
- Languages of a multi-language extension (MangaDex…) can be switched on and off from its settings.
- First run: the empty library points to Sources, and the extension index downloads by itself.
- The library card no longer goes back when you reopen an old chapter.

## 0.1.0 — 2026-11-22

First public release.
