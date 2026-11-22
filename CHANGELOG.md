# Changelog

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
