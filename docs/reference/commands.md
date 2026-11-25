# Commands

Every command has `--help` with examples, prints its result on stdout (as JSON with `--json`), and exits with a stable [exit code](exit-codes.md). This page covers the commands that are built so far: `init`, `key`, `check` and `repo`.

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
| `pentym key rotate` | a full slot | Generates a new key pair and re-wraps every data key to it (see below) |

`key add` flags:

| Flag | Meaning |
| --- | --- |
| `--key <reference>` | The new slot's passphrase; in a terminal it is asked for instead |
| `--age-recipient <age1…>` | Make an age slot for that identity instead of a passphrase slot |
| `--agent` | Make an agent slot, which can write backups but never read them |
| `--label <name>` | A name shown in `key list` |

The commands that unlock read the passphrase, recovery code or age identity from the terminal, or from `repository.key` when there is no terminal. `--slot <id>` tries only that slot, which saves one Argon2id derivation per other slot.

Every change is journaled with the secret reference used, never the secret.

### `pentym key rotate`

Generates a new repository key pair, re-wraps every data key in the index and every log segment's key to it, and keeps the replaced private key, encrypted, as `keys/private-<n>`. Packs are never rewritten, and slots are unchanged. It holds the exclusive lock and is journaled; if it is interrupted, everything stays readable and running it again finishes the work.

Rotation protects data written after it. Whoever holds a replaced private key can still read data written before: see [Key rotation](../crypto.md#key-rotation).

## `pentym check [target]`

Checks the repository's integrity (verify level L0):

| Step | Checks |
| --- | --- |
| slots | Every key slot parses; warns with one full slot ([PTM-4006](../errors/PTM-4006.md)); every replaced private key opens |
| index | Every index file matches its name and decrypts |
| packs | Every pack the index lists exists and is long enough; warns about unindexed packs and stray objects |
| snapshots | Every snapshot decrypts, its chunk lists read back, and every chunk it references is indexed |
| data | A sample of packs is downloaded whole: its hash matches its name, its trailer matches the index, and every chunk decrypts and matches its ID |
| segments | A sample of log segments is downloaded and decrypted |

| Flag | Meaning |
| --- | --- |
| `--sample <percent>` | The share of packs and log segments read in full; default 5, at least one of each |
| `--full` | Read every pack and log segment |
| `--slot <id>` | Unlock with this slot only |

With a target, only that target's snapshots, the packs they use and its log segments are checked.

`check` needs a full key slot, holds a shared lock so prune cannot delete what it reads, and is journaled. It prints a table with one row per step and a message per problem ([PTM-7001](../errors/PTM-7001.md) to [PTM-7004](../errors/PTM-7004.md)); `--json` prints the whole report. It exits with code 7 when it finds an error; warnings alone exit 0.

## `pentym repo`

| Command | Unlocks with | Does |
| --- | --- | --- |
| `pentym repo rebuild-index` | any slot | Rebuilds the index from the pack trailers and replaces the old index files |
| `pentym repo upgrade` | a full slot | Upgrades the repository to the newest format this pentym understands |

Both hold the exclusive lock and are journaled, and both take `--slot <id>`.

`rebuild-index` writes the new index files before deleting the old ones, so an interrupted rebuild loses nothing. Packs whose trailer cannot be read are listed, and the command exits with code 7 ([PTM-7002](../errors/PTM-7002.md)).

`upgrade` is the only thing that changes the repository format: upgrading pentym never does. Run it only after every pentym that uses the repository (agents, CLIs, CI jobs) is upgraded, because older versions refuse a newer format ([PTM-3001](../errors/PTM-3001.md)). In a terminal it lists the steps and asks for confirmation (default No; declining exits with code 9). Format 1 is the only format so far, so it reports that the repository is current.
