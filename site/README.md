# site — the project page

The page at wealthdb.smartpointer.com. It is static: one HTML file with
its fonts and images in `public/`. The Pages workflow publishes that
folder on every push to `main` that changes it.

The dashboard screenshots are not in git. A GitHub release named
`site-images` holds them, and the workflow downloads them into
`public/shots/` before it publishes. Each screenshot is the demo
household at a seed and date pinned in the Makefile, so a reshoot moves
only what the code changed.

```sh
make site-shots      # rebuild the demo at the pinned seed and date, shoot all dashboards
make site-publish    # upload the shots to the release and redeploy the page
make site-images     # download the published shots, for a local preview
```

`site-shots` also prints the figures the page's text quotes: the
terminal block and the agent conversation. Those stay hand-written, so
compare them before publishing.

`site-publish` needs a GitHub token that may write the repository's
contents and run its workflows. It reads `WEALTHDB_SITE_TOKEN`, from the
environment or from `~/.secrets/github-site.env`.

To preview the page locally:

```sh
make site-images
python3 -m http.server -d site/public 8000    # then open http://127.0.0.1:8000/
```
