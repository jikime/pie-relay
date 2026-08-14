const KROOT_DONE_TAG = '<kroot>DONE</kroot>'
const KROOT_DONE_PATTERN = /<kroot>\s*DONE\s*<\/kroot>/gi

// Relay journals keep Claude's original text. The browser removes only the
// Kroot completion control marker and withholds a trailing partial marker so a
// tag split across SSE chunks never flashes in the visible Markdown response.
export function filterAssistantMarkdown(value) {
  let visible = String(value ?? '').replace(KROOT_DONE_PATTERN, '')
  const normalizedTag = KROOT_DONE_TAG.toLowerCase()
  const maxPrefix = Math.min(visible.length, normalizedTag.length - 1)

  for (let length = maxPrefix; length > 0; length -= 1) {
    if (normalizedTag.startsWith(visible.slice(-length).toLowerCase())) {
      visible = visible.slice(0, -length)
      break
    }
  }
  return visible
}
