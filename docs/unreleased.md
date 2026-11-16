# Unreleased

- Add project session history to both clients and gateways: `minigw project sessions` for members and
  `minigw gw project-sessions --project` for the owner. Sessions group recorded top-level requests with nested calls,
  expose metadata and precise recorded totals, and follow paginated results. Readers see their own history;
  project admins and the authenticated owner can see the selected project's history. Existing usage reports,
  legacy session commands and the Mac Settings chart keep their current behavior.
