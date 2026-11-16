# Unreleased

- Add member Markdown export/import and owner team knowledge zip bundles on both clients. Durable receipts
  prevent duplicates, local edits are preserved, dry-run writes nothing, and bundle imports validate before
  one atomic transaction. Private bundle notes are opt-in; unknown users start inactive without bindings.
  Database backup and restore are unchanged.

- Add project session history to both clients and gateways: `minigw project sessions` for members and
  `minigw gw project-sessions --project` for the owner. Sessions group recorded top-level requests with nested calls,
  expose metadata and precise recorded totals, and follow paginated results. Readers see their own history;
  project admins and the authenticated owner can see the selected project's history. Existing usage reports,
  legacy session commands and the Mac Settings chart keep their current behavior.
