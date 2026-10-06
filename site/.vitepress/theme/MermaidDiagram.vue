<script lang="ts">
// Every drawing on a page needs an id of its own.
let drawings = 0
</script>

<script setup lang="ts">
// A Mermaid diagram of a page. The page is rendered on the server at build
// time, where there is no DOM to draw in, so the HTML carries the source as a
// code block and the browser replaces it with the drawing, and draws it again
// in the colours of the other theme when the reader switches.
import { computed, onMounted, ref, watch } from 'vue'
import { useData } from 'vitepress'

const props = defineProps<{ code: string }>()

const { isDark } = useData()
const source = computed(() => decodeURIComponent(props.code))
const svg = ref('')

let round = 0

function colour(name: string): string {
  return getComputedStyle(document.documentElement).getPropertyValue(name).trim()
}

async function draw() {
  const mine = ++round
  const { default: mermaid } = await import('mermaid')
  mermaid.initialize({
    startOnLoad: false,
    securityLevel: 'strict',
    theme: 'base',
    fontFamily: colour('--vp-font-family-base'),
    themeVariables: {
      darkMode: isDark.value,
      fontSize: '14px',
      background: colour('--pco-surface'),
      primaryColor: colour('--pco-raised'),
      primaryBorderColor: colour('--pco-edge'),
      primaryTextColor: colour('--pco-text'),
      secondaryColor: colour('--pco-accent-weak'),
      tertiaryColor: colour('--pco-bg'),
      lineColor: colour('--pco-edge'),
      textColor: colour('--pco-text'),
      clusterBkg: colour('--pco-bg'),
      clusterBorder: colour('--pco-border'),
      noteBkgColor: colour('--pco-warn-weak'),
      noteBorderColor: colour('--pco-warn'),
      noteTextColor: colour('--pco-text'),
    },
  })
  try {
    const { svg: drawing } = await mermaid.render(`pco-diagram-${++drawings}`, source.value)
    if (mine === round) svg.value = drawing
  } catch (error) {
    console.error('Mermaid could not draw a diagram:', error)
  }
}

onMounted(draw)
watch(isDark, draw)
</script>

<template>
  <div v-if="svg" class="pco-diagram" v-html="svg" />
  <div v-else class="language-mermaid"><pre><code>{{ source }}</code></pre></div>
</template>
