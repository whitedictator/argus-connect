package com.argus.connect

import java.time.Instant
import java.util.ArrayDeque

object DiagnosticJournal {
    private const val MAX_ENTRIES = 360
    private val entries = ArrayDeque<String>(MAX_ENTRIES)

    @Synchronized
    fun record(category: String, value: String) {
        val oneLine = value.replace('\n', ' ').take(4_000)
        entries.addLast("${Instant.now()} [$category] $oneLine")
        while (entries.size > MAX_ENTRIES) entries.removeFirst()
    }

    @Synchronized
    fun snapshot(): List<String> = entries.toList()

    fun redact(value: String): String = value
        .replace(Regex("\\b(?:\\d{1,3}\\.){3}\\d{1,3}\\b"), "[ip]")
        .replace(Regex("(?i)code[=: ]+\\d{6}"), "code=[redacted]")
}
