# PCに置くのはHTTP窓口とチャット用の配布資産だけ。
chat_sources() {
 git -C "$1" sparse-checkout set --no-cone --stdin <<'PATTERNS' || return 1
/chat-deploy.json
/chat-instructions.md
/skills/
/scripts/
!/scripts/verify.sh
!/scripts/api-manifest.ts
/shared/src/
/deploy/clients/remote-control/
/deploy/nas/update-skills.sh
/deploy/nas/update-skills-instructions.sh
/deploy/nas/update-remote-control.sh
/deploy/nas/update-remote-control-guard.sh
/deploy/nas/update-remote-control-single.sh
/deploy/nas/remote-control-settings.json
/deploy/nas/claude-rc@.service
/docs/chat-operations.md
PATTERNS
 git -C "$1" checkout --quiet
}

