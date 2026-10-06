// The id GitHub gives a heading, so that a link written for the pages on
// GitHub and in the package finds its heading on the site too: the text in
// lower case, without anything but letters, marks, digits, connector
// punctuation, hyphens and spaces, and each space a hyphen. A heading that
// repeats an earlier one gets -1, -2 and so on, from VitePress as from GitHub.
export function slugify(text) {
  return text
    .toLowerCase()
    .replace(/[^\p{L}\p{M}\p{N}\p{Pc}\- ]/gu, '')
    .replace(/ /g, '-')
}
