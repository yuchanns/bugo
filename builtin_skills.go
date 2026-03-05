package main

import "embed"

// builtinSkillsFS embeds repository bundled skills into the binary.
//
//go:embed skills/**
var builtinSkillsFS embed.FS
