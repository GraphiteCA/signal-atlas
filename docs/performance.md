# Performance measurements

These are historical measurements, not benchmarks of v0.3.2. No new performance runs were made for this documentation update.

## Large surveys

The GPU map adds exact clustering, a clustering toggle, label zoom controls and solid
planning rings. Radius defaults to 10 m and is adjustable from 5–50 m. Admin provides
survey switching/creation, backups, recoverable 0,0 cleanup and a read-only SQL browser.
Preparation prioritizes the current view while continuing the whole survey in the
background. Hidden APs use their BSSID/MAC identity, not a shared hidden-name label.

On the documented 5.75M-scan / 1.46M-target survey, the metadata follow-up reduced
complete GPU-fast preparation from 336 seconds to 149–151 seconds, with identical
saved results. This comparison is against the pre-metadata implementation, not an
upstream speedup claim. Final engine peaks were about 3.14 GiB; observation columns
remain resident and DuckDB remains an isolated benchmark prototype. First opening an
existing large survey can take longer once while internal metadata is migrated.

See the [review and validation guide](review-guide.md) for delivered features,
benchmark comparators, migrations, experimental boundaries and reproduction. The
[observation-quality findings](observation-quality.md) describe optional future
filters; they are not enabled as production defaults.

## Earlier upstream speed measurements

The following measurements predate source preservation (schema 6), do not include
its archival/provenance costs, and use a different 16-core PC
with 64 GB of memory. They are historical context, not new benchmark results.

| Pipeline | Rate |
|---|---|
| Explorer CSV import | 53–55 million rows a minute (50 million rows in 56 s) |
| Wardrive Compare, add and analyse | 76 million rows a minute |
| Explorer, opening a saved SQLite database | about 42 million rows a minute |
| Explorer, Fit every AP | about 13,000 networks a minute; 5 million rows (130,000 networks) in about 10 minutes, memory flat |
| Legacy Drive archive, adding logs | about 18 million rows a minute, the same at 10 million rows; under 0.2 GB of memory |
| Legacy Drive archive, adding a file already in it | about 50 million rows a minute (every row a duplicate) |
| Legacy Drive archive, exporting WiGLE CSV | about 58 million rows a minute, one file or one per day |
| Explorer queries | under 0.6 s at 10 million rows, under 3 s at 50 million |
| AP location fits | about 10,700 radios a minute |

The earlier implementation reported about 80 MB per million Compare rows and
150 MB per million survey observations. These are historical workload-specific
figures, not current memory budgets; source preservation adds disk and I/O costs.
