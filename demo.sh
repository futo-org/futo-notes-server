#!/usr/bin/env bash
# End-to-end acceptance script for the Stonefruit POC.
#
# Starts by assuming the server is running at BASE (default localhost:3000).
# Exits 0 if every step produces the expected HTTP status and payload shape.
#
# Requires: curl, jq.

set -uo pipefail

BASE="${BASE:-http://localhost:3000/api}"
ALICE_COOKIES="$(mktemp -t stonefruit-alice-cookies.XXXXXX)"
BOB_COOKIES="$(mktemp -t stonefruit-bob-cookies.XXXXXX)"
BLOB_FILE="$(mktemp -t stonefruit-blob.XXXXXX)"
head -c 256 /dev/urandom > "$BLOB_FILE"

cleanup() {
  rm -f "$ALICE_COOKIES" "$BOB_COOKIES" "$BLOB_FILE"
}
trap cleanup EXIT

pass=0
fail=0
step=0

# step <name> <expected_status> <actual_status> [extra check message]
step() {
  step=$((step + 1))
  local name="$1"
  local expected="$2"
  local actual="$3"
  if [ "$expected" = "$actual" ]; then
    echo "  PASS  [$step] $name (HTTP $actual)"
    pass=$((pass + 1))
  else
    echo "  FAIL  [$step] $name — expected HTTP $expected, got $actual"
    fail=$((fail + 1))
  fi
}

# http METHOD URL COOKIE_JAR [JSON_BODY] [--binary FILE] [--save-cookies]
# Writes the response body to $BODY and sets $STATUS.
http() {
  local method="$1" url="$2" jar="$3" body_type="" body="" save_cookies="" ctype=""
  shift 3
  while [ $# -gt 0 ]; do
    case "$1" in
      --json) body_type="json"; body="$2"; ctype="application/json"; shift 2 ;;
      --binary) body_type="binary"; body="$2"; ctype="application/octet-stream"; shift 2 ;;
      --save-cookies) save_cookies="1"; shift ;;
      *) echo "unknown arg: $1"; exit 2 ;;
    esac
  done

  local tmp
  tmp="$(mktemp)"
  local curl_args=(-sS -o "$tmp" -w '%{http_code}' -X "$method" -b "$jar")
  [ -n "$save_cookies" ] && curl_args+=(-c "$jar")
  [ -n "$ctype" ] && curl_args+=(-H "Content-Type: $ctype")

  if [ "$body_type" = "json" ]; then
    curl_args+=(-d "$body")
  elif [ "$body_type" = "binary" ]; then
    curl_args+=(--data-binary "@$body")
  fi

  STATUS="$(curl "${curl_args[@]}" "$url")"
  BODY="$(cat "$tmp")"
  rm -f "$tmp"
}

echo "Stonefruit POC acceptance test — target: $BASE"
echo

# 1. Alice logs in.
http POST "$BASE/auth/dev/login" "$ALICE_COOKIES" \
  --json '{"email":"alice@test.com","name":"Alice"}' --save-cookies
step "Alice login" 200 "$STATUS"
ALICE_ID=$(printf '%s' "$BODY" | jq -r .user.id)

# 2. Alice creates a collection.
http POST "$BASE/collections" "$ALICE_COOKIES"
step "Alice creates collection" 201 "$STATUS"
COLLECTION_ID=$(printf '%s' "$BODY" | jq -r .collection.id)

# 3. Alice uploads an opaque blob.
http POST "$BASE/blobs" "$ALICE_COOKIES" --binary "$BLOB_FILE"
step "Alice uploads blob" 201 "$STATUS"
BLOB_KEY=$(printf '%s' "$BODY" | jq -r .key)
if [[ "$BLOB_KEY" == "$ALICE_ID/"* ]]; then
  echo "        blob key is scoped to Alice: $BLOB_KEY"
else
  echo "  FAIL  blob key not scoped to Alice: $BLOB_KEY"
  fail=$((fail + 1))
fi

# 4. Alice creates an object pointing at that blob.
http POST "$BASE/collections/$COLLECTION_ID/objects" "$ALICE_COOKIES" \
  --json "{\"blob_key\":\"$BLOB_KEY\",\"size_bytes\":256}"
step "Alice creates object (v1)" 201 "$STATUS"
OBJECT_ID=$(printf '%s' "$BODY" | jq -r .object.id)

# 5. Alice PUTs version 2.
http PUT "$BASE/collections/$COLLECTION_ID/objects/$OBJECT_ID" "$ALICE_COOKIES" \
  --json "{\"version\":2,\"blob_key\":\"$BLOB_KEY\",\"size_bytes\":256}"
step "Alice updates to v2" 200 "$STATUS"

# 6. Alice PUTs version 2 again — should conflict.
http PUT "$BASE/collections/$COLLECTION_ID/objects/$OBJECT_ID" "$ALICE_COOKIES" \
  --json "{\"version\":2,\"blob_key\":\"$BLOB_KEY\",\"size_bytes\":256}"
step "Stale v2 PUT returns 409" 409 "$STATUS"
CURRENT_VERSION=$(printf '%s' "$BODY" | jq -r .currentVersion)
if [ "$CURRENT_VERSION" = "2" ]; then
  echo "        conflict response reported currentVersion=2"
else
  echo "  FAIL  conflict response wrong currentVersion: $CURRENT_VERSION"
  fail=$((fail + 1))
fi

# 7. Alice pulls since version 0 — should see the v2 object.
http GET "$BASE/collections/$COLLECTION_ID/objects?sinceVersion=0" "$ALICE_COOKIES"
step "Alice pulls sinceVersion=0" 200 "$STATUS"
COUNT=$(printf '%s' "$BODY" | jq '.objects | length')
VERSION=$(printf '%s' "$BODY" | jq -r '.objects[0].version')
if [ "$COUNT" = "1" ] && [ "$VERSION" = "2" ]; then
  echo "        pull returned 1 object at version 2"
else
  echo "  FAIL  pull response unexpected: count=$COUNT version=$VERSION"
  fail=$((fail + 1))
fi

# 8. Bob logs in (separate cookie jar).
http POST "$BASE/auth/dev/login" "$BOB_COOKIES" \
  --json '{"email":"bob@test.com","name":"Bob"}' --save-cookies
step "Bob login" 200 "$STATUS"

# 9. Bob tries Alice's collection — should get 404 (no existence leak).
http GET "$BASE/collections/$COLLECTION_ID" "$BOB_COOKIES"
step "Bob blocked from Alice's collection (404)" 404 "$STATUS"

# 10. Bob tries Alice's blob — should get 404.
http GET "$BASE/blobs/$BLOB_KEY" "$BOB_COOKIES"
step "Bob blocked from Alice's blob (404)" 404 "$STATUS"

# 11. Bob tries Alice's object via its parent collection — should get 404.
http GET "$BASE/collections/$COLLECTION_ID/objects/$OBJECT_ID" "$BOB_COOKIES"
step "Bob blocked from Alice's object (404)" 404 "$STATUS"

# 12. Unauthenticated access to a protected route — 401.
UNAUTH_JAR="$(mktemp -t stonefruit-unauth.XXXXXX)"
http GET "$BASE/collections" "$UNAUTH_JAR"
rm -f "$UNAUTH_JAR"
step "Unauthenticated request returns 401" 401 "$STATUS"

# 13. Alice fetches her own blob bytes and confirms round-trip integrity.
RECEIVED="$(mktemp)"
curl -sS -b "$ALICE_COOKIES" "$BASE/blobs/$BLOB_KEY" -o "$RECEIVED"
if cmp -s "$BLOB_FILE" "$RECEIVED"; then
  step "Alice blob round-trip byte-equal" 0 0
else
  step "Alice blob round-trip byte-equal" 0 1
fi
rm -f "$RECEIVED"

echo
echo "--- Notes-client scenarios ---"
echo "Simulating how a notes app layers on top of this generic sync service:"
echo "  * one collection per vault"
echo "  * one object per note body, one per attachment, one for the client-encrypted index"
echo "  * edits re-upload ciphertext + PUT a new version"
echo "  * deletes are soft, so sync peers can drop their local copy"
echo

# Fresh collection so version thresholds are easy to reason about.
http POST "$BASE/collections" "$ALICE_COOKIES"
step "Alice creates a notes vault (collection)" 201 "$STATUS"
VAULT_ID=$(printf '%s' "$BODY" | jq -r .collection.id)

# Three blobs — in reality each would be ciphertext produced by the client.
NOTE_BODY_FILE="$(mktemp)";  head -c 1024 /dev/urandom > "$NOTE_BODY_FILE"
ATTACHMENT_FILE="$(mktemp)"; head -c 4096 /dev/urandom > "$ATTACHMENT_FILE"
INDEX_FILE="$(mktemp)";      head -c 256  /dev/urandom > "$INDEX_FILE"

http POST "$BASE/blobs" "$ALICE_COOKIES" --binary "$NOTE_BODY_FILE"
NOTE_BODY_KEY=$(printf '%s' "$BODY" | jq -r .key)
http POST "$BASE/blobs" "$ALICE_COOKIES" --binary "$ATTACHMENT_FILE"
ATTACHMENT_KEY=$(printf '%s' "$BODY" | jq -r .key)
http POST "$BASE/blobs" "$ALICE_COOKIES" --binary "$INDEX_FILE"
INDEX_KEY=$(printf '%s' "$BODY" | jq -r .key)

# 14. Three objects: the note body, one attachment, and the index metadata.
http POST "$BASE/collections/$VAULT_ID/objects" "$ALICE_COOKIES" \
  --json "{\"blob_key\":\"$NOTE_BODY_KEY\",\"size_bytes\":1024}"
step "Create note-body object" 201 "$STATUS"
NOTE_ID=$(printf '%s' "$BODY" | jq -r .object.id)

http POST "$BASE/collections/$VAULT_ID/objects" "$ALICE_COOKIES" \
  --json "{\"blob_key\":\"$ATTACHMENT_KEY\",\"size_bytes\":4096}"
step "Create attachment object" 201 "$STATUS"
ATTACH_ID=$(printf '%s' "$BODY" | jq -r .object.id)

http POST "$BASE/collections/$VAULT_ID/objects" "$ALICE_COOKIES" \
  --json "{\"blob_key\":\"$INDEX_KEY\",\"size_bytes\":256}"
step "Create client-encrypted index-metadata object" 201 "$STATUS"
INDEX_ID=$(printf '%s' "$BODY" | jq -r .object.id)

# 15. Fresh-device bootstrap: pull everything from v=0. Sees all 3 at v=1.
http GET "$BASE/collections/$VAULT_ID/objects?sinceVersion=0" "$ALICE_COOKIES"
step "Fresh device pulls entire vault" 200 "$STATUS"
COUNT=$(printf '%s' "$BODY" | jq '.objects | length')
if [ "$COUNT" = "3" ]; then
  echo "        bootstrap returned 3 objects at v=1 (note + attachment + index)"
else
  echo "  FAIL  expected 3 objects, got $COUNT"
  fail=$((fail + 1))
fi

# 16. Edit the note. Client re-encrypts the body, uploads fresh ciphertext,
#     and PUTs the object to v=2.
EDITED_FILE="$(mktemp)"; head -c 1100 /dev/urandom > "$EDITED_FILE"
http POST "$BASE/blobs" "$ALICE_COOKIES" --binary "$EDITED_FILE"
EDITED_KEY=$(printf '%s' "$BODY" | jq -r .key)
http PUT "$BASE/collections/$VAULT_ID/objects/$NOTE_ID" "$ALICE_COOKIES" \
  --json "{\"version\":2,\"blob_key\":\"$EDITED_KEY\",\"size_bytes\":1100}"
step "Alice edits note body (v1 -> v2)" 200 "$STATUS"

# 17. Alice soft-deletes the attachment. Version bumps; deleted=true.
http DELETE "$BASE/collections/$VAULT_ID/objects/$ATTACH_ID" "$ALICE_COOKIES"
step "Alice soft-deletes the attachment" 200 "$STATUS"
DEL_VERSION=$(printf '%s' "$BODY" | jq -r .object.version)
DEL_FLAG=$(printf '%s' "$BODY" | jq -r .object.deleted)
if [ "$DEL_VERSION" = "2" ] && [ "$DEL_FLAG" = "true" ]; then
  echo "        tombstone at version=2, deleted=true"
else
  echo "  FAIL  unexpected delete response: version=$DEL_VERSION deleted=$DEL_FLAG"
  fail=$((fail + 1))
fi

# 18. Incremental sync: a second device had cursor=1 after the bootstrap.
#     Pulling sinceVersion=1 should yield exactly the two changed objects:
#     the edited note body and the deleted-attachment tombstone. Not the
#     untouched index object.
http GET "$BASE/collections/$VAULT_ID/objects?sinceVersion=1" "$ALICE_COOKIES"
step "Second device incremental sync returns only changed objects" 200 "$STATUS"
COUNT=$(printf '%s' "$BODY" | jq '.objects | length')
HAS_EDITED=$(printf '%s' "$BODY" | jq --arg id "$NOTE_ID"   '[.objects[] | select(.id == $id and (.version|tonumber) == 2 and .deleted == false)] | length')
HAS_TOMBSTONE=$(printf '%s' "$BODY" | jq --arg id "$ATTACH_ID" '[.objects[] | select(.id == $id and (.version|tonumber) == 2 and .deleted == true)]  | length')
HAS_UNCHANGED=$(printf '%s' "$BODY" | jq --arg id "$INDEX_ID"  '[.objects[] | select(.id == $id)] | length')
if [ "$COUNT" = "2" ] && [ "$HAS_EDITED" = "1" ] && [ "$HAS_TOMBSTONE" = "1" ] && [ "$HAS_UNCHANGED" = "0" ]; then
  echo "        delta = {edited note v=2, attachment tombstone v=2}; unchanged index not re-sent"
else
  echo "  FAIL  delta mismatch: count=$COUNT edited=$HAS_EDITED tombstone=$HAS_TOMBSTONE unchanged=$HAS_UNCHANGED"
  fail=$((fail + 1))
fi

# 19. Round-trip the edited note body. Client would decrypt this locally;
#     here we just confirm the server returned the ciphertext byte-equal.
RECEIVED_EDIT="$(mktemp)"
curl -sS -b "$ALICE_COOKIES" "$BASE/blobs/$EDITED_KEY" -o "$RECEIVED_EDIT"
if cmp -s "$EDITED_FILE" "$RECEIVED_EDIT"; then
  step "Edited note body round-trips byte-equal" 0 0
else
  step "Edited note body round-trips byte-equal" 0 1
fi
rm -f "$RECEIVED_EDIT"

# 20. Vault isolation. Alice's *original* test collection (from step 2)
#     must not contain any of the notes-vault objects. Protects against
#     leakage if a client sends the wrong collection id on pull.
http GET "$BASE/collections/$COLLECTION_ID/objects?sinceVersion=0" "$ALICE_COOKIES"
step "Pull of original collection excludes notes-vault objects" 200 "$STATUS"
LEAKED=$(printf '%s' "$BODY" | jq --arg a "$NOTE_ID" --arg b "$ATTACH_ID" --arg c "$INDEX_ID" \
  '[.objects[] | select(.id == $a or .id == $b or .id == $c)] | length')
if [ "$LEAKED" = "0" ]; then
  echo "        vault A pull cleanly excludes vault B's objects"
else
  echo "  FAIL  $LEAKED notes-vault object(s) leaked into original collection pull"
  fail=$((fail + 1))
fi

rm -f "$NOTE_BODY_FILE" "$ATTACHMENT_FILE" "$INDEX_FILE" "$EDITED_FILE"

echo
echo "========================================"
echo "  $pass passed, $fail failed"
echo "========================================"

[ "$fail" -eq 0 ]
