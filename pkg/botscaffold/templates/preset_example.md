---
name: example
display_name: Example Preset
description: A starter sous-bot — adapt or delete this file
# vars: override any variable your workflow declares (precedence: defaults < preset < --var)
# vars:
#   some_var: "a value"
# skills: bundle skills (skills/<name>.md) the agent should consult under this preset
# skills: [my-skill]
---
Replace this body with the launch-time bias for this preset. It is appended to
every LLM node's system prompt under a "## Focus" section, so describe the lens
the bot should adopt when an operator selects `--preset example` (or picks it in
the studio Launch dialog).
