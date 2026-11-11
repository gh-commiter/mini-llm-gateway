//go:build !linux

package main

// followStops: le Linux (waitid); Windows nima ustavitve, minigw na macOS je aplikacijski (Swift, main.swift).
func followStops(int) {}
