# Image for the test:client job: Go, Rust, and the client's integration tests
# already compiled, so a pipeline only builds the server. The client is pinned
# because the tests are named exactly; bump CLIENT_COMMIT (and the image tag in
# .gitlab-ci.yml) deliberately.
FROM golang:1.27-bookworm

ARG CLIENT_COMMIT=6db8de0e

RUN apt-get update \
    && apt-get install -y --no-install-recommends build-essential curl git jq postgresql-client ripgrep \
    && rm -rf /var/lib/apt/lists/*

ENV RUSTUP_HOME=/opt/rustup CARGO_HOME=/opt/cargo PATH=/opt/cargo/bin:$PATH
RUN curl --proto '=https' --tlsv1.2 -fsSL https://sh.rustup.rs | sh -s -- -y --profile minimal

ENV FUTO_NOTES_CLIENT_REPO=/opt/client
RUN git clone https://gitlab.futo.org/futo-notes/futo-notes.git "$FUTO_NOTES_CLIENT_REPO" \
    && git -C "$FUTO_NOTES_CLIENT_REPO" checkout "$CLIENT_COMMIT" \
    && cd "$FUTO_NOTES_CLIENT_REPO" \
    && cargo test -p futo-notes-sync --test server_integration --test sse_live --no-run
