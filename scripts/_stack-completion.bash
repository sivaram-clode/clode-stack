# clode-stack/scripts/_stack-completion.bash
#
# Defines the `stack` shell function and a bash-completion routine for it.
# Source this from ~/.bashrc:
#
#   source /home/kong/Desktop/Internship/clode/clode-stack/scripts/_stack-completion.bash
#
# The `stack` function always invokes the wrapper by absolute path, so the
# subcommand works from any cwd. Each underlying script in scripts/ already
# `cd`s to the project root, so no relative-path footguns.

# ── absolute path to the stack root (resolved when this file is sourced) ──
__STACK_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
__STACK_BIN="${__STACK_ROOT}/stack.sh"

stack() {
  command "${__STACK_BIN}" "$@"
}

# ── completion ────────────────────────────────────────────────────────────
_stack_complete() {
  local cur prev words cword
  _init_completion -n : 2>/dev/null || {
    # _init_completion is from bash-completion; fall back if it's missing.
    cur="${COMP_WORDS[COMP_CWORD]}"
    prev="${COMP_WORDS[COMP_CWORD-1]}"
    words=("${COMP_WORDS[@]}")
    cword=$COMP_CWORD
  }

  local subcmds="up down wipe cleanup reseed seed tail-logs build-cache help"
  local sub="${words[1]:-}"

  # Subcommand position.
  if (( cword == 1 )); then
    COMPREPLY=( $(compgen -W "$subcmds" -- "$cur") )
    return
  fi

  # Service names — parsed live from docker-compose.yml so additions show up
  # without editing this file. Falls back to the canonical list if the file
  # is unreadable.
  __stack_services() {
    if [[ -r "${__STACK_ROOT}/docker-compose.yml" ]]; then
      awk '
        /^services:/      { in_services = 1; next }
        in_services && /^[^[:space:]]/ { in_services = 0 }
        in_services && /^  [a-zA-Z0-9_-]+:[[:space:]]*$/ {
          name = $1; sub(":", "", name); print name
        }
      ' "${__STACK_ROOT}/docker-compose.yml"
    else
      echo db redis minio minio-setup databend raksha jumbo brahmi \
           pool-manager chil toolkit-proxy cha-ching skills-registry \
           mang-proxy cloudflared
    fi
  }

  # Profile names — `profiles: [foo, bar]` lines under any service block.
  # Dedupe + sort, since multiple services often share a profile.
  __stack_profiles() {
    [[ -r "${__STACK_ROOT}/docker-compose.yml" ]] || return
    awk '
      /^[[:space:]]*profiles:[[:space:]]*\[/ {
        # inline-array form: profiles: [a, b]
        line = $0
        sub(/.*\[/, "", line); sub(/\].*/, "", line)
        n = split(line, parts, /[[:space:]]*,[[:space:]]*/)
        for (i = 1; i <= n; i++) {
          p = parts[i]; gsub(/^[[:space:]"'\'']+|[[:space:]"'\'']+$/, "", p)
          if (p != "") print p
        }
      }
      /^[[:space:]]*-[[:space:]]/ && in_profiles {
        # block-list form under profiles:
        p = $0; sub(/^[[:space:]]*-[[:space:]]*/, "", p)
        gsub(/["'\''[:space:]]/, "", p)
        if (p != "") print p
      }
      /^[[:space:]]*profiles:[[:space:]]*$/ { in_profiles = 1; next }
      /^[[:space:]]*[a-zA-Z0-9_-]+:[[:space:]]*$/ && in_profiles { in_profiles = 0 }
    ' "${__STACK_ROOT}/docker-compose.yml" | sort -u
  }

  # Per-service-DB names that cleanup --postgres accepts as positional args.
  local cleanup_dbs="raksha jumbo brahmi pool-manager chil_new chaching skills_registry"
  local cleanup_flags="--postgres --redis --redis-mang --databend --minio --agents -a --all --reseed -n --dry-run -y --yes -h --help"

  case "$sub" in
    up|tail-logs|logs)
      local profiles_list
      profiles_list="$(__stack_profiles)"

      # --profile=foo,bar — CSV-aware: complete the segment after the last comma.
      if [[ "$cur" == --profile=* ]]; then
        local val="${cur#--profile=}"
        local prefix="${val%,*}"
        local frag="${val##*,}"
        if [[ "$val" == *,* ]]; then
          COMPREPLY=( $(compgen -P "--profile=${prefix}," -W "$profiles_list" -- "$frag") )
        else
          COMPREPLY=( $(compgen -P "--profile=" -W "$profiles_list" -- "$val") )
        fi
        compopt -o nospace 2>/dev/null
        return
      fi
      # After `--profile ` (space form) — also CSV-aware.
      if [[ "$prev" == "--profile" ]]; then
        if [[ "$cur" == *,* ]]; then
          local prefix="${cur%,*}" frag="${cur##*,}"
          COMPREPLY=( $(compgen -P "${prefix}," -W "$profiles_list" -- "$frag") )
        else
          COMPREPLY=( $(compgen -W "$profiles_list" -- "$cur") )
        fi
        return
      fi
      # Flags this subcommand accepts.
      if [[ "$cur" == -* ]]; then
        COMPREPLY=( $(compgen -W "--batch --profile" -- "$cur") )
        return
      fi
      # Otherwise positional args are service names; allow repeats.
      COMPREPLY=( $(compgen -W "$(__stack_services)" -- "$cur") )
      ;;
    cleanup|clean-up|clean)
      if [[ "$cur" == -* ]]; then
        COMPREPLY=( $(compgen -W "$cleanup_flags" -- "$cur") )
      else
        COMPREPLY=( $(compgen -W "$cleanup_dbs" -- "$cur") )
      fi
      ;;
    down)
      COMPREPLY=()
      ;;
    wipe)
      # --yes/-y skips the confirmation prompt; --dry-run/-n previews only.
      COMPREPLY=( $(compgen -W "--yes -y --dry-run -n --help -h" -- "$cur") )
      ;;
    reseed)
      # reseed forwards flags to cleanup.sh (already has -y --reseed set).
      if [[ "$cur" == -* ]]; then
        COMPREPLY=( $(compgen -W "$cleanup_flags" -- "$cur") )
      else
        COMPREPLY=( $(compgen -W "$cleanup_dbs" -- "$cur") )
      fi
      ;;
    seed|build-cache|help)
      COMPREPLY=()
      ;;
    *)
      COMPREPLY=( $(compgen -W "$subcmds" -- "$cur") )
      ;;
  esac
}

complete -F _stack_complete stack
