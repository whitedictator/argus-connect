package com.argus.connect

import android.content.Context
import java.io.File
import java.time.Instant
import java.util.ArrayDeque
import java.util.concurrent.Executors
import java.util.concurrent.TimeUnit
import java.util.concurrent.atomic.AtomicBoolean

/** Bounded local history. No networking and no message or packet payloads. */
object DiagnosticJournal {
    private const val MAX_ENTRIES = 360
    private val entries = ArrayDeque<String>(MAX_ENTRIES)
    private val scheduled = AtomicBoolean(false)
    private val writer = Executors.newSingleThreadScheduledExecutor { task -> Thread(task,"argus-diagnostics").apply { isDaemon=true } }
    @Volatile private var history: File? = null

    @Synchronized
    fun initialize(context: Context) {
        if (history != null) return
        val file = File(context.filesDir,"diagnostics/journal.txt")
        file.parentFile?.mkdirs()
        runCatching {
            if (file.isFile && file.length() <= 2 * 1024 * 1024) {
                file.readLines().takeLast(MAX_ENTRIES).forEach { entries.addLast(it.take(4_200)) }
            }
        }
        history = file
    }

    @Synchronized
    fun record(category: String, value: String) {
        val oneLine = redact(value.replace('\n',' ')).take(4_000)
        entries.addLast("${Instant.now()} [$category] $oneLine")
        while (entries.size > MAX_ENTRIES) entries.removeFirst()
        if (history != null && scheduled.compareAndSet(false,true)) {
            writer.schedule({
                scheduled.set(false)
                val text = snapshot().joinToString("\n",postfix="\n")
                val file = history ?: return@schedule
                runCatching {
                    val temp = File(file.parentFile,"journal.new")
                    temp.writeText(text)
                    check(temp.renameTo(file))
                }
            },2,TimeUnit.SECONDS)
        }
    }

    @Synchronized fun snapshot(): List<String> = entries.toList()

    fun redact(value: String): String = value
        .replace(Regex("\\b(?:\\d{1,3}\\.){3}\\d{1,3}\\b"),"[ip]")
        .replace(Regex("(?i)code[=: ]+\\d{6}"),"code=[redacted]")
        .replace(Regex("(?i)([?&](?:secret|token|password|key|code)=)[^&\\s]+"),"\$1[redacted]")
}
