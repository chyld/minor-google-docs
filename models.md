# Agent team

**alpha** is the primary agent; it writes and builds, and delegates review work to **beta**.

| # | Team | Provider | Model | Reasoning |
|---|---|---|---|---|
| 0 | alpha | Claude | `claude-opus-5-5` | — (primary; delegates to beta) |
| 1 | beta | Claude | `claude-sonnet-5-5` | high |
| 2 | beta | Codex | `gpt-6.1-sol` | high |
| 3 | beta | Grok | `grok-4.7` | high |
| 4 | beta | Muse Code | `muse-wonder-1.0` | high |
| 5 | beta | OpenCode Go | `opencode-go/deepseek-v4.1-flash` | high |
| 6 | beta | OpenCode Go | `opencode-go/minimax-m3.1-flash` | high |
| 7 | beta | OpenCode Go | `opencode-go/glm-5.3-flash` | high |
| 8 | beta | OpenCode Go | `opencode-go/kimi-k3.1-pro` | high |

Every beta model runs at **high** reasoning, set explicitly. The OpenCode models use the paid
`opencode-go` tier, not the free models. Kimi K3.1 Pro replaces Mimo (`mimo-v2.6`), which is no
longer on the team.

## How the team works

1. **alpha** writes the spec.
2. **beta** reviews the spec; alpha folds the findings in.
3. **alpha** builds it.
4. **beta** code-reviews; alpha fixes what holds up, one commit per review.

Reviewers run on the same machine, so each one uses its own port and never kills a process it
didn't start. Never `pkill` by name: that once killed other reviewers' servers.
