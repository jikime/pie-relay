import { z } from 'zod'

// IPC channel names live in a zod-free module so the sandboxed preload can import
// them without pulling zod. Re-exported here for existing Main-side importers.
export {
  PIE_CHAT_ADD_REACTION_CHANNEL,
  PIE_CHAT_CREATE_ATTACHMENT_INTENT_CHANNEL,
  PIE_CHAT_CREATE_CHANNEL_CHANNEL,
  PIE_CHAT_CREATE_DM_CHANNEL,
  PIE_CHAT_CREATE_GROUP_DM_CHANNEL,
  PIE_CHAT_DELETE_MESSAGE_CHANNEL,
  PIE_CHAT_DOWNLOAD_ATTACHMENT_CHANNEL,
  PIE_CHAT_EDIT_MESSAGE_CHANNEL,
  PIE_CHAT_GET_PRESENCE_CHANNEL,
  PIE_CHAT_LIST_CHANNELS_CHANNEL,
  PIE_CHAT_LIST_MEMBERS_CHANNEL,
  PIE_CHAT_LIST_MESSAGES_CHANNEL,
  PIE_CHAT_LIST_NOTIFICATIONS_CHANNEL,
  PIE_CHAT_LIST_PINS_CHANNEL,
  PIE_CHAT_MARK_ALL_NOTIFICATIONS_READ_CHANNEL,
  PIE_CHAT_MARK_NOTIFICATION_READ_CHANNEL,
  PIE_CHAT_MARK_READ_CHANNEL,
  PIE_CHAT_MESSAGES_CHANGED_CHANNEL,
  PIE_CHAT_MUTE_CHANNEL_CHANNEL,
  PIE_CHAT_PIN_MESSAGE_CHANNEL,
  PIE_CHAT_PRESENCE_CHANGED_CHANNEL,
  PIE_CHAT_REMOVE_REACTION_CHANNEL,
  PIE_CHAT_SEARCH_MESSAGES_CHANNEL,
  PIE_CHAT_SEND_MESSAGE_CHANNEL,
  PIE_CHAT_SEND_TYPING_CHANNEL,
  PIE_CHAT_TYPING_CHANGED_CHANNEL,
  PIE_CHAT_UNMUTE_CHANNEL_CHANNEL,
  PIE_CHAT_UNPIN_MESSAGE_CHANNEL
} from './pie-chat-ipc-channels'

// Resource types the chat surface reacts to (subset of the realtime union). A
// realtime resource.changed of any of these nudges the timeline to refetch;
// reactions/pins/edits/deletes all arrive as a 'message' change.
export const PIE_CHAT_REALTIME_RESOURCE_TYPES = new Set<string>([
  'channel',
  'channel_member',
  'message',
  'read_cursor',
  'notification'
])

const opaqueIdSchema = z.string().uuid()

export const ChannelVisibilitySchema = z.enum(['internal', 'project', 'customer'])
export const ChannelKindSchema = z.enum(['channel', 'dm'])

// Mirrors ChannelResource from @pie/persistence. Passthrough so an additive
// optional field from a newer control-plane does not fail client validation.
export const PieChannelSchema = z
  .object({
    id: opaqueIdSchema,
    organizationId: opaqueIdSchema,
    name: z.string(),
    kind: ChannelKindSchema,
    scopeType: z.string(),
    scopeId: z.string().nullable(),
    visibility: ChannelVisibilitySchema,
    version: z.number().int(),
    createdAt: z.string(),
    updatedAt: z.string(),
    // Unread messages for the requesting user; present only on the channel list.
    unreadCount: z.number().int().optional()
  })
  .passthrough()

export const PieMessageReactionSchema = z
  .object({
    emoji: z.string(),
    count: z.number().int(),
    reactedByMe: z.boolean()
  })
  .passthrough()

export const PieMessageAttachmentSchema = z
  .object({
    id: z.string(),
    filename: z.string(),
    contentType: z.string(),
    byteSize: z.number().int()
  })
  .passthrough()

// Mirrors MessageResource from @pie/persistence, including the edited/tombstone
// and OCC version fields the timeline renders. Passthrough for forward-compat.
export const PieMessageSchema = z
  .object({
    id: opaqueIdSchema,
    organizationId: opaqueIdSchema,
    channelId: opaqueIdSchema,
    authorId: opaqueIdSchema,
    body: z.string(),
    visibility: ChannelVisibilitySchema,
    version: z.number().int(),
    threadRootMessageId: z.string().nullable(),
    replyCount: z.number().int(),
    reactions: z.array(PieMessageReactionSchema),
    attachments: z.array(PieMessageAttachmentSchema),
    createdAt: z.string(),
    edited: z.boolean(),
    revisionCount: z.number().int(),
    deleted: z.boolean(),
    deletedAt: z.string().nullable(),
    deletedBy: z.string().nullable(),
    deletionReason: z.string().nullable(),
    pinned: z.boolean()
  })
  .passthrough()

export const PieChannelListResponseSchema = z
  .object({
    items: z.array(PieChannelSchema),
    nextCursor: z.string().nullable()
  })
  .passthrough()

export const PieMessageListResponseSchema = z
  .object({
    items: z.array(PieMessageSchema),
    nextCursor: z.string().nullable()
  })
  .passthrough()

export const PieChatMessagesChangedSchema = z
  .object({
    type: z.literal('chat.messages-changed'),
    organizationId: opaqueIdSchema
  })
  .passthrough()

// Ephemeral collaboration pushes forwarded from the realtime connection to the
// trusted renderer. Non-durable: the payload IS the state (typing self-clears on
// a TTL, presence on the next presence event); they carry no cursor/version.
export const PieChatTypingChangedSchema = z
  .object({
    type: z.literal('chat.typing-changed'),
    organizationId: opaqueIdSchema,
    channelId: opaqueIdSchema,
    userId: opaqueIdSchema,
    at: z.string()
  })
  .passthrough()

export const PieChatPresenceChangedSchema = z
  .object({
    type: z.literal('chat.presence-changed'),
    organizationId: opaqueIdSchema,
    userId: opaqueIdSchema,
    state: z.enum(['online', 'offline']),
    at: z.string()
  })
  .passthrough()

export const PieChatListMessagesOptionsSchema = z
  .object({
    limit: z.number().int().min(1).max(200).optional(),
    cursor: opaqueIdSchema.optional(),
    threadRoot: opaqueIdSchema.optional()
  })
  .strict()

// Extra POST-message fields. Body stays plain text; mention targets ride
// out-of-band as user ids (backend resolves + drops non-members), and
// attachmentIds link previously-uploaded objects at post time.
export const PieSendMessageOptionsSchema = z
  .object({
    threadRootMessageId: opaqueIdSchema.optional(),
    mentions: z.array(opaqueIdSchema).max(100).optional(),
    mentionChannel: z.boolean().optional(),
    mentionHere: z.boolean().optional(),
    attachmentIds: z.array(z.string()).max(10).optional()
  })
  .strict()

export const PiePinnedMessageSchema = z
  .object({
    message: PieMessageSchema,
    pinnedBy: z.string(),
    pinnedAt: z.string()
  })
  .passthrough()

export const PiePinListResponseSchema = z
  .object({ items: z.array(PiePinnedMessageSchema) })
  .passthrough()

export const PieMessageSearchResponseSchema = z
  .object({
    items: z.array(PieMessageSchema),
    nextCursor: z.string().nullable()
  })
  .passthrough()

// One org member the composer can @-mention and the sidebar can DM.
export const PieChatMemberSchema = z
  .object({
    userId: opaqueIdSchema,
    displayName: z.string()
  })
  .passthrough()

export const PieAttachmentIntentSchema = z
  .object({
    id: z.string(),
    objectId: z.string(),
    uploadUrl: z.string(),
    expiresAt: z.string()
  })
  .passthrough()

export const PieAttachmentDownloadSchema = z
  .object({
    url: z.string(),
    filename: z.string(),
    contentType: z.string(),
    expiresAt: z.string()
  })
  .passthrough()

// One durable per-user notification (currently only type 'mention'). Mirrors
// NotificationResource from @pie/persistence. `type` stays a plain string so a
// future notification kind does not fail client validation. channelId/messageId
// reference the mention's location; the actor + message body are NOT on this
// resource (backend only stores the reference), so the inbox shows type +
// channel + relative time, not an author line or the message text.
export const PieNotificationSchema = z
  .object({
    id: opaqueIdSchema,
    organizationId: opaqueIdSchema,
    userId: opaqueIdSchema,
    type: z.string(),
    channelId: z.string().nullable(),
    messageId: z.string().nullable(),
    seen: z.boolean(),
    read: z.boolean(),
    createdAt: z.string()
  })
  .passthrough()

export const PieNotificationListResponseSchema = z
  .object({
    items: z.array(PieNotificationSchema),
    nextCursor: z.string().nullable()
  })
  .passthrough()

// The read-all route returns the count of rows it flipped to read.
export const PieNotificationsReadAllResponseSchema = z
  .object({ updated: z.number().int() })
  .passthrough()

export type ChannelVisibility = z.infer<typeof ChannelVisibilitySchema>
export type ChannelKind = z.infer<typeof ChannelKindSchema>
export type PieChannel = z.infer<typeof PieChannelSchema>
export type PieMessage = z.infer<typeof PieMessageSchema>
export type PieMessageReaction = z.infer<typeof PieMessageReactionSchema>
export type PieMessageAttachment = z.infer<typeof PieMessageAttachmentSchema>
export type PieChannelListResponse = z.infer<typeof PieChannelListResponseSchema>
export type PieMessageListResponse = z.infer<typeof PieMessageListResponseSchema>
export type PieChatMessagesChanged = z.infer<typeof PieChatMessagesChangedSchema>
export type PieChatTypingChanged = z.infer<typeof PieChatTypingChangedSchema>
export type PieChatPresenceChanged = z.infer<typeof PieChatPresenceChangedSchema>
export type PieChatListMessagesOptions = z.infer<typeof PieChatListMessagesOptionsSchema>
export type PieSendMessageOptions = z.infer<typeof PieSendMessageOptionsSchema>
export type PiePinnedMessage = z.infer<typeof PiePinnedMessageSchema>
export type PiePinListResponse = z.infer<typeof PiePinListResponseSchema>
export type PieMessageSearchResponse = z.infer<typeof PieMessageSearchResponseSchema>
export type PieChatMember = z.infer<typeof PieChatMemberSchema>
export type PieAttachmentIntent = z.infer<typeof PieAttachmentIntentSchema>
export type PieAttachmentDownload = z.infer<typeof PieAttachmentDownloadSchema>
export type PieNotification = z.infer<typeof PieNotificationSchema>
export type PieNotificationListResponse = z.infer<typeof PieNotificationListResponseSchema>

// Renderer-facing bridge. It never carries tokens or the org/user ids the renderer
// should not hold — Main resolves those from the auth lifecycle + session broker.
export type PieChatRendererApi = {
  listChannels: () => Promise<PieChannel[]>
  listMessages: (
    channelId: string,
    opts?: PieChatListMessagesOptions
  ) => Promise<PieMessageListResponse>
  sendMessage: (
    channelId: string,
    body: string,
    opts?: PieSendMessageOptions
  ) => Promise<PieMessage>
  editMessage: (
    channelId: string,
    messageId: string,
    body: string,
    expectedVersion: number
  ) => Promise<PieMessage>
  deleteMessage: (channelId: string, messageId: string) => Promise<void>
  markRead: (channelId: string, lastReadMessageId: string) => Promise<void>
  addReaction: (channelId: string, messageId: string, emoji: string) => Promise<PieMessage>
  removeReaction: (channelId: string, messageId: string, emoji: string) => Promise<void>
  pinMessage: (channelId: string, messageId: string) => Promise<void>
  unpinMessage: (channelId: string, messageId: string) => Promise<void>
  listPins: (channelId: string) => Promise<PiePinnedMessage[]>
  createChannel: (name: string, visibility?: ChannelVisibility) => Promise<PieChannel>
  createDm: (otherUserId: string) => Promise<PieChannel>
  createGroupDm: (participantUserIds: string[]) => Promise<PieChannel>
  muteChannel: (channelId: string) => Promise<void>
  unmuteChannel: (channelId: string) => Promise<void>
  searchMessages: (query: string, cursor?: string) => Promise<PieMessageSearchResponse>
  listMembers: () => Promise<PieChatMember[]>
  // Uploads bytes and returns the attachment intent whose id links the file to a
  // subsequent sendMessage via opts.attachmentIds. Both the intent and the
  // presigned PUT happen in Main.
  uploadAttachment: (
    channelId: string,
    meta: { filename: string; contentType: string; byteSize: number },
    file: ArrayBuffer
  ) => Promise<PieAttachmentIntent>
  downloadAttachment: (channelId: string, attachmentId: string) => Promise<PieAttachmentDownload>
  // The caller's own durable notification feed (mentions). markAll returns the
  // count flipped to read so the renderer can zero its unread badge optimistically.
  listNotifications: () => Promise<PieNotificationListResponse>
  markNotificationRead: (notificationId: string) => Promise<PieNotification>
  markAllNotificationsRead: () => Promise<number>
  onMessagesChanged: (callback: (event: PieChatMessagesChanged) => void) => () => void
  // Fire-and-forget typing ping; the backend rate-coalesces per user/channel.
  sendTyping: (channelId: string) => Promise<void>
  // The currently-online user ids, to seed presence when the chat surface mounts
  // after Main already received the initial presence burst.
  getPresenceSnapshot: () => Promise<string[]>
  // Live ephemeral collaboration signals (no cursor/version); the payload IS the
  // state, so the renderer applies each directly and self-heals on a TTL.
  onTypingChanged: (callback: (event: PieChatTypingChanged) => void) => () => void
  onPresenceChanged: (callback: (event: PieChatPresenceChanged) => void) => () => void
}
