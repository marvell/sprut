# MCP server only, no shell interface for tools

Sprut exposes Upstream tools only over MCP. It has no `sprut call <tool>` style command that would let a bash-only Agent invoke tools from the shell. A shell interface would cost little context and work with any Agent, but without a daemon every call would cold-start the Upstream, and it doubles the surface area of v0.1.
