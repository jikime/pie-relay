export type MeetingDisplaySource = {
  id: string
  name: string
  kind: 'screen' | 'window'
  thumbnailDataUrl: string
}

export type MeetingMediaPreloadApi = {
  listDisplaySources: () => Promise<MeetingDisplaySource[]>
  selectDisplaySource: (sourceId: string) => Promise<boolean>
}
