# Minimalist OpenSpec Schema (v2)

`minimalist` is for getting to build quickly with a direct `specs -> tasks` flow.

- Good fit: landing pages and simple apps with too many technical design decisions.
- Not a good fit: complete apps with multiple layers, database work, and broader architecture concerns.

## Install (copy/paste)

Use the root `README.md` single-line install command with:
- `SCHEMA="minimalist"`

## Activate

Set this in `openspec/config.yaml`:

```yaml
schema: minimalist
```

## Spec Format

When authoring `specs` artifacts in this schema:
- Use `## ADDED/MODIFIED/REMOVED Requirements` delta sections and `### Requirement:` headings.
- Keep the user story (`As a <role>, I want <capability>, so that <benefit>.`) in the requirement body and include a SHALL/MUST statement.
- Write each acceptance criterion as a named `#### Scenario:` with GIVEN/WHEN/THEN.
- MODIFIED contains the complete requirement and all its scenarios; omit unused delta sections.
- Include a meaningful `## Purpose` for a new capability; omit it for an existing one. Archive reads it only when creating a baseline.

This local v2 adaptation uses the CLI's Requirements/Scenario format so validation and archive work with OpenSpec 1.13.0. The original v1 User Stories/Acceptance Criteria headings are not supported by that CLI. Existing v1 changes need an explicit artifact migration and review before finalization; do not bypass validation.

## Associated Skills

This schema declares its companion skills in `skills.txt`; they are installed automatically by Step 6 of `AGENT_INSTALL.md` into `.agents/skills/`, sourced from [intent-driven-dev/skills](https://github.com/intent-driven-dev/skills).

- `openspec-git-discipline` — git hygiene for OpenSpec propose/apply/archive workflows.

For more schemas, refer to https://github.com/intent-driven-dev/openspec-schemas.
