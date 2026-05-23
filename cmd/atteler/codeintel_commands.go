package main

// Code-intel command handlers are split by concern:
// codeintel_command_descriptors.go owns selector/help metadata;
// codeintel_schema.go, codeintel_response_builders.go, and
// codeintel_response_render.go own the typed output contract; and the
// codeintel_*_commands.go files own query/filter helpers by domain.
