### Fixed
- **Calibre import no longer binds to a different work by title** — a Calibre book whose title matched another work by the same author that already had its own Calibre id (e.g. "Mistborn" vs "Mistborn: Secret History") used to attach its files to that work and overwrite its `calibre_id`. It now gets its own row (#3631).
