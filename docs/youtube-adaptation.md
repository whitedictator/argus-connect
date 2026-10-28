# Router findings applied to Android

The accepted router module on 2026-10-28 improved Shorts through staggered,
same-origin ClientHello retries and fallback for unanswered QUIC. Good TLS on
an unrelated CDN did not prove that a signed video request would work there.

Android port in this alpha:

- Only exact GoogleVideo-host TLS ClientHello records are eligible.
- At most four staggered attempts to the same remote IP/port; existing tactic first.
- No retries of application requests, no TLS interception, and no 0-RTT replay.
- Truncated, coalesced and early_data ClientHellos retain the old forwarding path.
- A global budget limits simultaneous handshake races to four.
- The winning socket alone receives subsequent application traffic.
- Existing Telegram, WhatsApp, Signal and RVX-specific paths are retained.

The router's NFQUEUE, SO_MARK, tcp_md5 fake packets and nftables rules are not
available to an ordinary rootless Android app and are not claimed as implemented.
Endpoint-scoped QUIC recovery still needs separate carrier tests; this release
does not introduce a new global QUIC kill switch.

Local tests prove parsing, cancellation and no application replay. They do not
prove faster playback on a specific carrier: new videos, Shorts, seeking and
sustained playback must be checked on the actual phone network.
