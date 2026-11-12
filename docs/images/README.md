# Documentation screenshots

These are real captures of Signal Atlas v0.3.2 (`03ceeb9`), taken on
2026-11-12 using its local `-serve` mode in Chrome at a 1440 × 1000 CSS-pixel
viewport. Map captures show the full app viewport; Compare and import history
are direct captures of the relevant page sections. No results or controls were
painted into the images. The native window frame is not shown.

All observations and network identities are synthetic. The map is Vancouver,
British Columbia, on inland downtown blocks near Burrard and Robson. Approximate demo
trajectories are not road-accurate GPS tracks, navigation routes or evidence of
any actual device at a location. MAC addresses use the locally administered
`02:CA:FE` prefix; SSIDs start with `DEMO-`.

| Image | Demonstrates |
|---|---|
| [Survey map](vancouver-survey-map.png) | Sightings, all 120 fitted AP locations, planning rings and completed background preparation |
| [AP details](vancouver-ap-details.png) | Fictional AP identity, fit details, labels and pinning |
| [Compare](vancouver-compare.png) | Two survey-derived drives, shared route and baseline metrics |
| [Import history](vancouver-import-history.png) | Observation counts, duplicate accounting and retained originals |

## Recreate the sample

From the repository root, generate the two deterministic CSV logs in a new
directory (the generator refuses to overwrite existing files):

```bash
python scripts/docs-sample.py --out build/docs-capture/logs
```

Use a separate empty survey workspace and import both logs. The sample produces
12,373 and 9,739 source rows, including 302 and 238 intentional duplicates.
The resulting survey has 21,572 observations and 120 fictional networks.
Allow the default reference preparation to finish before capturing the map.

The revised fixture places transmitters inside six inland blocks and collects
intersecting passes around them. Every one of the 120 calculated locations was
checked to lie inside that inland grid before capture; no estimates were removed
or repositioned for the images. This is a controlled synthetic demonstration,
not validation of location accuracy on real-world surveys.

For the map overview, show sightings and shared APs, turn clustering off, and set planning
rings to 10 m. All 120 fitted locations are visible in the overview, centered near
longitude -123.1227, latitude 49.2828 at zoom 15.6. The detail view uses the same
center at zoom 16.6, with labels
from zoom 16 and a sample AP selected and pinned.

For Compare, choose **From the Atlas survey**, one drive per day, and keep
duplicate restoration enabled. Name the drives **Vancouver demo - morning** and
**Vancouver demo - repeat**. Capture **Routes and shared road** with its baseline
table. Import history is in **Admin → Import scans**. Import timestamps and
automatic baseline selection can differ between captures.

Generated logs, workspaces, browser profiles and executables belong under the
ignored `build/` directory. Only these images and the sample generator are
included in the repository. Keep synthetic-data captions when reusing images.

Basemap data is © [OpenStreetMap contributors](https://www.openstreetmap.org/copyright).
The maps retain their on-screen attribution.
