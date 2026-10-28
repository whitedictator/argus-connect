package com.argus.connect

import android.content.Context
import android.content.Intent
import android.net.ConnectivityManager
import datapath.Datapath
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.launch
import kotlinx.coroutines.Job
import kotlinx.coroutines.delay
import java.net.HttpURLConnection
import java.net.URL
import java.util.concurrent.ConcurrentHashMap
import java.util.concurrent.CopyOnWriteArraySet

class LightController(private val context: Context) {
    private val listeners = CopyOnWriteArraySet<(LightState) -> Unit>()
    private val scope = CoroutineScope(SupervisorJob() + Dispatchers.IO)
    private val baselineResults = ConcurrentHashMap<String, CheckResult>()
    private var baselineGeneration = 0L
    private val baselineJobs = mutableListOf<Job>()
    private val baselineConnections = ConcurrentHashMap<HttpURLConnection, Long>()

    @Volatile private var state = LightState()
    @Volatile private var diagnosticNativeState = "{}"

    fun nativeStatus(): String = diagnosticNativeState
    fun recordNativeStatus(encoded: String) { diagnosticNativeState = encoded }

    init {
        analyzeCurrentNetwork()
    }

    fun current(): LightState = state

    fun proxyPool(): String = context.getSharedPreferences(PREFERENCES, Context.MODE_PRIVATE)
        .getString(KEY_PROXY_POOL, "").orEmpty()

    fun proxyCount(): Int = proxyPool().lineSequence().count { it.isNotBlank() }

    fun publicMTProxyDiscovery(): Boolean = context.getSharedPreferences(PREFERENCES, Context.MODE_PRIVATE)
        .getBoolean("mtproxy_public_discovery", false)

    fun saveProxyPool(value: String, discover: Boolean = publicMTProxyDiscovery()): Int {
        val accepted = value.lineSequence()
            .map(String::trim)
            .filter { it.startsWith("socks5://", true) || it.startsWith("socks5h://", true) || it.startsWith("tg://socks?", true) || it.startsWith("tg://proxy?") || it.startsWith("https://t.me/proxy?") || Regex("wss://kws[1-5]\\.(pclead|cakeisalie)\\.co\\.uk/apiws").matches(it) }
            .distinct()
            .take(MAX_PROXY_COUNT)
            .toList()
        context.getSharedPreferences(PREFERENCES, Context.MODE_PRIVATE)
            .edit().putString(KEY_PROXY_POOL, accepted.joinToString("\n"))
            .putBoolean("mtproxy_public_discovery", discover).apply()
        DiagnosticJournal.record("proxy", "Fallback pool saved count=${accepted.size} publicMTProxyDiscovery=$discover")
        if (state.isRunning || state.isBusy) stop("Прокси сохранены — включите LIGHT снова")
        return accepted.size
    }

    fun observe(listener: (LightState) -> Unit): AutoCloseable {
        listeners += listener
        listener(state)
        return AutoCloseable { listeners -= listener }
    }

    @Synchronized
    fun start() {
        if (state.phase == LightPhase.ANALYZING) cancelBaseline()
        if (state.isBusy || state.isRunning) return
        publish(LightState(LightPhase.STARTING, "Запуск LIGHT", "Поднимаю локальный защищённый datapath…"))
        context.startForegroundService(Intent(context, ArgusLightService::class.java))
    }

    @Synchronized
    fun stop(reason: String = "LIGHT выключен") {
        if (state.phase == LightPhase.ANALYZING) {
            cancelBaseline()
            publish(LightState(title = "Анализ остановлен", detail = "Можно включить LIGHT"))
            return
        }
        if (state.phase == LightPhase.OFF || state.phase == LightPhase.STOPPING) return
        publish(state.copy(phase = LightPhase.STOPPING, title = "Остановка LIGHT", detail = "Закрываю локальный VPN…"))
        context.startService(
            Intent(context, ArgusLightService::class.java)
                .setAction(ArgusLightService.ACTION_STOP)
                .putExtra(ArgusLightService.EXTRA_REASON, reason),
        )
    }

    private data class BaselineProfile(
        val id: String,
        val service: String,
        val target: String,
        val minimumBytes: Int = 0,
    )

    @Synchronized
    private fun cancelBaseline() {
        baselineGeneration++
        baselineJobs.forEach { it.cancel() }
        baselineJobs.clear()
        val connections = baselineConnections.keys.toList()
        connections.forEach { baselineConnections.remove(it) }
        scope.launch { connections.forEach { runCatching { it.disconnect() } } }
        if (state.phase == LightPhase.ANALYZING) state = LightState()
    }

    @Synchronized
    fun analyzeCurrentNetwork() {
        if (state.isRunning || state.phase == LightPhase.STARTING || state.phase == LightPhase.STOPPING) return
        cancelBaseline()
        val generation = baselineGeneration
        val profiles = listOf(
            BaselineProfile("telegram", "telegram", ""),
            BaselineProfile("whatsapp-messages", "whatsapp", "https://www.whatsapp.com/robots.txt"),
            BaselineProfile("whatsapp-media-route", "whatsapp", "https://mmg.whatsapp.net/"),
            BaselineProfile("signal", "signal", "https://chat.signal.org/"),
            BaselineProfile("threema", "threema", "https://ds-apip.threema.ch/"),
            BaselineProfile("youtube-feed", "youtube", "https://www.youtube.com/", minimumBytes = 32 * 1024),
            BaselineProfile(
                "youtube-thumbnails",
                "youtube",
                "https://i.ytimg.com/vi/jNQXAC9IVRw/hqdefault.jpg",
                minimumBytes = 5 * 1024,
            ),
            BaselineProfile("youtube-video-route", "youtube", "https://redirector.googlevideo.com/report_mapping?di=no"),
        )
        baselineResults.clear()
        publish(
            LightState(
                phase = LightPhase.ANALYZING,
                title = "Анализирую текущую сеть",
                detail = "Проверяю 8 независимых маршрутов до включения обхода",
                recoveringServices = profiles.map { it.id }.toSet(),
            ),
        )
        profiles.forEach { profile ->
            baselineJobs += scope.launch {
                val result = if (directProbe(profile, generation)) CheckResult.WORKS else CheckResult.FAILED
                synchronized(this@LightController) {
                    if (generation == baselineGeneration && state.phase == LightPhase.ANALYZING) {
                        baselineResults[profile.id] = result
                        publishBaseline(profiles.map { it.id })
                    }
                }
            }
        }
        baselineJobs += scope.launch {
            delay(20_000)
            synchronized(this@LightController) {
                if (generation == baselineGeneration && state.phase == LightPhase.ANALYZING) {
                    val partial = state
                    cancelBaseline()
                    publish(partial.copy(phase = LightPhase.BASELINE, title = "Анализ завершён по тайм-ауту", detail = "Незавершённые проверки не означают блокировку. Можно включить LIGHT", recoveringServices = emptySet(), automaticChecksComplete = false))
                }
            }
        }
    }

    private fun directProbe(profile: BaselineProfile, generation: Long): Boolean {
        val connectivity = context.getSystemService(ConnectivityManager::class.java)
        val network = connectivity.activeNetwork ?: return false
        if (profile.service == "telegram") {
            return runCatching { Datapath.probeTelegramNetwork(network.networkHandle) }.getOrDefault(false)
        }
        var connection: HttpURLConnection? = null
        return runCatching {
            connection = network.openConnection(URL(profile.target)) as HttpURLConnection
            synchronized(this) {
                if (generation != baselineGeneration) return false
                baselineConnections[connection!!] = generation
            }
            SignalTrust.configure(context, connection!!)
            connection!!.useCaches = false
            connection!!.setRequestProperty("Cache-Control", "no-cache, no-store, max-age=0")
            connection!!.setRequestProperty("Pragma", "no-cache")
            connection!!.setRequestProperty("Connection", "close")
            if (profile.minimumBytes == 0) connection!!.setRequestProperty("Range", "bytes=0-0")
            connection!!.instanceFollowRedirects = false
            connection!!.connectTimeout = BASELINE_TIMEOUT_MS
            connection!!.readTimeout = BASELINE_TIMEOUT_MS
            connection!!.requestMethod = "GET"
            connection!!.connect()
            val code = connection!!.responseCode
            val body = if (code >= 400) connection!!.errorStream else connection!!.inputStream
            var received = 0
            body?.use { input ->
                val buffer = ByteArray(8 * 1024)
                var remaining = 64 * 1024
                while (remaining > 0) {
                    val count = input.read(buffer, 0, minOf(buffer.size, remaining))
                    if (count < 0) break
                    received += count
                    remaining -= count
                }
            }
            code in 100..599 && received >= profile.minimumBytes
        }.getOrDefault(false).also { connection?.let { baselineConnections.remove(it); it.disconnect() } }
    }

    @Synchronized
    private fun publishBaseline(services: List<String>) {
        if (state.phase != LightPhase.ANALYZING) return
        fun result(service: String) = baselineResults[service] ?: CheckResult.WAITING
        val remaining = services.filterNot(baselineResults::containsKey).toSet()
        val working = baselineResults.values.count { it == CheckResult.WORKS }
        val failed = baselineResults.filterValues { it == CheckResult.FAILED }.keys
        val youtubeFeed = result("youtube-feed")
        val youtubeThumbnails = result("youtube-thumbnails")
        val youtubeVideoRoute = result("youtube-video-route")
        val youtube = when {
            listOf(youtubeFeed, youtubeThumbnails, youtubeVideoRoute).any { it == CheckResult.FAILED } -> CheckResult.FAILED
            listOf(youtubeFeed, youtubeThumbnails, youtubeVideoRoute).all { it == CheckResult.WORKS } -> CheckResult.WORKS
            else -> CheckResult.WAITING
        }
        publish(
            state.copy(
                phase = if (remaining.isEmpty()) LightPhase.BASELINE else LightPhase.ANALYZING,
                title = if (remaining.isEmpty()) "Исходный тест завершён: $working/8" else "Анализирую текущую сеть",
                detail = when {
                    remaining.isNotEmpty() -> "Уже проверено ${baselineResults.size}/8 • без обхода"
                    failed.isEmpty() -> "Все 8 маршрутов отвечают; видео, медиа и звонки подтверждаются при использовании"
                    else -> "Без обхода недоступны: ${failed.map(::serviceLabel).joinToString(", ")}"
                },
                telegram = ServiceCheck(if (result("telegram") == CheckResult.WORKS) CheckResult.ROUTE else result("telegram")),
                whatsapp = ServiceCheck(result("whatsapp-messages")),
                whatsappMedia = when (result("whatsapp-media-route")) {
                    CheckResult.WORKS -> CheckResult.ROUTE
                    else -> result("whatsapp-media-route")
                },
                signal = ServiceCheck(result("signal")),
                threema = ServiceCheck(result("threema")),
                youtube = youtube,
                youtubeFeed = youtubeFeed,
                youtubeThumbnails = youtubeThumbnails,
                youtubeVideo = when (youtubeVideoRoute) {
                    CheckResult.WORKS -> CheckResult.ROUTE
                    else -> youtubeVideoRoute
                },
                automaticChecksComplete = remaining.isEmpty(),
                recoveringServices = remaining,
                failureReasons = failed.associateWith { "не отвечает без обхода" },
            ),
        )
    }

    private fun serviceLabel(service: String): String = when (service) {
        "telegram" -> "Telegram"
        "whatsapp-messages" -> "WhatsApp сообщения"
        "whatsapp-media-route" -> "WhatsApp медиа"
        "signal" -> "Signal"
        "threema" -> "Threema"
        "youtube-feed" -> "YouTube лента"
        "youtube-thumbnails" -> "YouTube обложки"
        "youtube-video-route" -> "YouTube видео"
        else -> service
    }

    internal fun publish(next: LightState) {
        state = next
        DiagnosticJournal.record(
            "light",
            "phase=${next.phase} dns=${next.dnsTransport}/${next.dohReady} " +
                "attempts=${next.probeAttempts.toSortedMap()} current=${next.currentStrategies.toSortedMap()} " +
                "routes=${next.currentRoutes.toSortedMap()} failures=${next.failureReasons.toSortedMap()}",
        )
        listeners.forEach { listener -> runCatching { listener(next) } }
        ArgusTileService.refresh(context)
    }

    companion object {
        private const val BASELINE_TIMEOUT_MS = 6_000
        private const val PREFERENCES = "argus_light"
        private const val KEY_PROXY_POOL = "proxy_pool"
        private const val MAX_PROXY_COUNT = 12
    }
}
