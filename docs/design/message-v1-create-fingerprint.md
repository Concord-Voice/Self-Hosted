# Message-v1 create fingerprint

## Status

This is a dormant codec for Security #177. It has no route, WebSocket handler,
database writer, reservation lookup, notification, or client call site. A later
writer must use the codec only after its current authorization, membership,
epoch, reply, and attachment-ownership checks complete.

## Canonical identity

`FingerprintV1` validates its input and hashes the exact UTF-8 bytes of this
whitespace-free positional JSON array with SHA-256:

```text
["concord-message-create",1,"message-v1",scope,scope_id,message_id,sender_id,key_epoch,"message-body",envelope,reply_to_id_or_empty,gif_slug_or_empty,attachment_ids,[mentioned_user_ids,mentioned_role_ids,everyone,here]]
```

`scope` is `channel` or `dm`. Every UUID is lowercase canonical RFC form, is
non-nil, and has an RFC variant. The initial protocol accepts UUID versions 1
through 8; first-party clients generate a CSPRNG UUIDv4. `key_epoch` is in
`1..9007199254740991`. `envelope` is an exact `m1:` prefix followed by canonical
standard padded base64 of at least a 12-byte nonce and a 16-byte tag.

`reply_to_id` is a canonical UUID or the empty string. A GIF slug follows the
existing normalization and validation rules; an absent or whitespace-only slug
becomes the empty string. Attachments preserve caller order, contain at most five
canonical UUIDs, and reject duplicates.

Mention user and role identifiers are validated, reject duplicates, then sort
lexicographically because their request order has no message meaning. The codec
binds requested mention intent, never later RBAC or delivery results. A channel
may bind users, roles, `everyone`, and `here`. A DM may bind users and `here`;
roles and `everyone` reject rather than being silently cleared.

## Future writer ordering

The codec is not a replay classifier. A future WebSocket-only message-v1 writer
must authenticate the sender, validate the canonical inputs, check the global
reservation rollout gate, lock and recheck current authorization and epoch,
then compute this fingerprint. Within that same transaction it must reserve or
inspect the global message ID, return the existing row only for the same stored
fingerprint, and otherwise use a uniform conflict for changed, deleted,
cross-scope, and legacy records. It commits message, attachment links, metadata,
and fingerprint before acknowledgements, broadcasts, unread updates, mention
routing, or DM effects.

REST remains legacy until it has equivalent message-v1 attachments, mention
intent, and side-effect semantics. It must reject attempted message-v1 fields
rather than silently storing them through a legacy create path.
