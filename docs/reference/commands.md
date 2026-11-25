# Commands

Every command has `--help` with examples, prints its result on stdout (as JSON with `--json`), and exits with a stable [exit code](exit-codes.md). This page covers the commands that are built so far.

Secrets never go on the command line. Flags that take a passphrase take a [secret reference](config.md#secrets) such as `env:NAME` or `file:/path`, and refuse a plain value. In a terminal, pentym asks for passphrases instead, without echoing them.

## `pentym init`

Creates an encrypted repository at `repository.path` (see [Configuration](config.md#repository)) with:

- at least two **full key slots**, or one full slot and a **paper recovery code**: losing the only way to unlock would make every backup unreadable;
- an **agent slot**, which can write backups but never read them.

```sh
pentym init
pentym init --key env:PENTYM_KEY --key file:/root/second-key --agent-key env:PENTYM_AGENT_KEY
pentym init --key env:PENTYM_KEY --paper --agent-key env:PENTYM_AGENT_KEY
```

| Flag | Meaning |
| --- | --- |
| `--key <reference>` | Add a full passphrase slot; repeatable |
| `--age-recipient <age1…>` | Add a full slot unlocked by that age identity; repeatable |
| `--paper` | Add a full slot unlocked by a paper recovery code, printed once |
| `--agent-key <reference>` | The agent slot's passphrase |
| `--insecure-test-mode` | Create an unencrypted repository for local development; no slots |

In a terminal, `init` asks for whatever the flags leave out: the first passphrase, then a second one (leave it empty to get a paper recovery code instead), then the agent slot's passphrase. Each new passphrase is typed twice.

`init` refuses:

- fewer than two full slots counting the paper code ([PTM-4005](../errors/PTM-4005.md));
- two full slots with the same passphrase;
- an agent passphrase equal to a full slot's, because a database host holding it could then unlock everything;
- a location that already holds a repository ([PTM-3003](../errors/PTM-3003.md)).

It also tests that the storage honours conditional create, by creating the same object twice, and warns if it does not ([PTM-3005](../errors/PTM-3005.md)).

The paper recovery code is printed on stdout once and stored nowhere: write it down and keep it offline. It is never logged or journaled.

`--print-sql` and `--print-mongo` are not built yet.

## `pentym key`

Manages key slots: the ways to unlock the repository. Every slot opens the same keys; removing a slot stops that way of unlocking.

| Command | Unlocks with | Does |
| --- | --- | --- |
| `pentym key list` | nothing | Lists the slots; warns when only one full slot is left |
| `pentym key add` | a full slot | Adds a passphrase slot, or an age slot with `--age-recipient` |
| `pentym key remove <slot>` | a full slot | Removes a slot; the last full slot cannot be removed ([PTM-4002](../errors/PTM-4002.md)) |
| `pentym key export --paper` | a full slot | Adds a paper slot and prints its recovery code once |
| `pentym key rotate` | — | Not built yet |

`key add` flags:

| Flag | Meaning |
| --- | --- |
| `--key <reference>` | The new slot's passphrase; in a terminal it is asked for instead |
| `--age-recipient <age1…>` | Make an age slot for that identity instead of a passphrase slot |
| `--agent` | Make an agent slot, which can write backups but never read them |
| `--label <name>` | A name shown in `key list` |

The commands that unlock read the passphrase, recovery code or age identity from the terminal, or from `repository.key` when there is no terminal. `--slot <id>` tries only that slot, which saves one Argon2id derivation per other slot.

Every change is journaled with the secret reference used, never the secret.
