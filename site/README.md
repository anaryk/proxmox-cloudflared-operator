# The documentation site

A VitePress build of the pages in `docs/`, published at
<https://anaryk.github.io/proxmox-cloudflared-operator/>. The package ships
`docs/` and not this directory, and each page is written to read as well on
GitHub and as plain text as it does here.

## Commands

From the repository root:

- `make docs` builds the site into `site/.vitepress/dist`. It fails on a link
  to a page that does not exist.
- `make docs-serve` serves it with live reload.
- `make docs-lint` holds the pages to the rules of `.markdownlint-cli2.jsonc`,
  refuses the syntax that only the site reads, checks every link of the built
  site down to its fragment, and checks the images against their budget.
- `make docs-test` runs every test of the site; it is `npm test` in this
  directory. `make test-scripts` runs the ones that need no packages.

`node --test scripts` does not work on Node 22, which reads a directory as a
module to run. Use the commands above, or give `node --test` the files.

## What a page may use

Markdown as GitHub reads it, and nothing that only the site renders: no
`:::` container, `[[toc]]` or `<<<` include (`docs-lint` refuses them), and no
raw HTML (markdownlint's MD033 refuses it). A `{{` is shown as the text it is,
in prose, code spans and indented blocks as in fences. Headings get the ids
GitHub gives them, so a link written for GitHub finds its heading here.

## Navigation

`sections` in `.vitepress/config.mts` lists the sections in order and the
pages in each. A page in `guides/`, `use-cases/` or `cli/` appears by itself,
under the directory's `index.md`, in the order of its title. A page with any
other name in `docs/` has to be added to `sections`, or the build stops and
says so.

## The pins that look odd

VitePress is the 2.0 alpha, pinned exactly. The `latest` on npm is 1.6.4, which
builds with Vite 5, and the advisories of that Vite, one of them rated high,
would show in the weekly audit (`npm audit --omit=dev`): VitePress is a
dependency here, not a dev dependency, since the site is built from it. The
alpha builds with Vite 8 and the audit finds nothing. The lock file fixes it,
the site is built on every push, and what comes out is static HTML, so a bad
bump shows up in CI and never on the site. Move to 2.x when it is `latest`.

`katex` is overridden to 0.18.10 in `package.json`. Mermaid depends on
`katex ^0.16`, and every 0.16 release falls under GHSA-238p-pmpm-9mq7. Mermaid
loads katex for `$$` in a diagram label and calls one function that 0.18.10
has. Drop the override when Mermaid allows katex 0.18. (0.18.11 is deprecated,
which is why the override names 0.18.10.)
