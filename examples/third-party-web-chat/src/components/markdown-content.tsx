'use client'

import { useLayoutEffect, useRef } from 'react'

import { renderMarkdown } from '../../public/markdown.js'

export function MarkdownContent({ source }: { source: string }) {
  const target = useRef<HTMLDivElement>(null)
  useLayoutEffect(() => {
    if (target.current) renderMarkdown(target.current, source)
  }, [source])
  return <div ref={target} />
}
