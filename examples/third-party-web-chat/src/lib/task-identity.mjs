function stringValue(value) {
  return typeof value === 'string' ? value : ''
}

// A background task outlives the main chat turn. Its requestId can therefore
// be temporarily absent during replay or reconnect, while taskId and
// parentToolUseId remain stable. Keep the visual identity independent of the
// request so one subagent always updates one card.
export function stableTaskIdentity(data, sequence, fallbackID) {
  const parentToolUseID = stringValue(data?.parentToolUseId)
  const taskID = stringValue(data?.taskId)
    || parentToolUseID
    || `task-${sequence || fallbackID}`
  return {
    parentToolUseID,
    taskID,
    id: `task-${parentToolUseID || taskID}`,
  }
}
