# Braces

Vue reads `{{ ... }}` as an expression, and GitHub shows it as it is.

In prose: {{c d}}, {{name}} and {{ 1 + 1 }}, also ${{ github.ref }}.

In a code span: `{{c d}}`, `{{name}}` and `{{ 1 + 1 }}`, also `{{ .Name }}`.

In emphasis and a link: *{{c d}}* and [{{name}}](https://example.com/ "{{ 1 + 1 }}").

Written as references: &#123;&#123;c d}} and &#123;&#123; 1 + 1 &#125;&#125;.

| Where | Value |
| --- | --- |
| prose | {{c d}} {{name}} {{ 1 + 1 }} |
| code | `{{c d}}` `{{name}}` `{{ 1 + 1 }}` |

- a list item with {{c d}} and `{{name}}`

> a quote with {{ 1 + 1 }}

    {{c d}}
    {{name}}
    {{ 1 + 1 }}

```text
{{c d}}
{{name}}
{{ 1 + 1 }}
```

## A heading with {{ 1 + 1 }}

The end.
