package com.argus.connect

import android.app.Notification
import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.content.Context
import android.content.Intent
import android.net.ConnectivityManager
import android.net.Network
import android.net.NetworkCapabilities
import android.net.NetworkRequest
import android.net.VpnService
import android.os.IBinder
import android.os.ParcelFileDescriptor
import datapath.Datapath
import datapath.Engine
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.Job
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.cancel
import kotlinx.coroutines.delay
import kotlinx.coroutines.joinAll
import kotlinx.coroutines.launch
import kotlinx.coroutines.withTimeoutOrNull
import kotlinx.coroutines.currentCoroutineContext
import kotlinx.coroutines.ensureActive
import org.json.JSONObject
import java.io.ByteArrayOutputStream
import java.net.HttpURLConnection
import java.net.URL
import java.util.concurrent.ConcurrentHashMap
import java.util.concurrent.atomic.AtomicBoolean

class ArgusLightService : VpnService() {
    private val scope = CoroutineScope(SupervisorJob() + Dispatchers.IO)
    private val stopping = AtomicBoolean(false)
    private val controller get() = (application as ArgusApplication).light
    private val connectivity get() = getSystemService(ConnectivityManager::class.java)

    private var tun: ParcelFileDescriptor? = null
    private var engine: Engine? = null
    private var underlying: Network? = null
    private var networkCallback: ConnectivityManager.NetworkCallback? = null
    private var statusJob: Job? = null
    private var probeSuiteJob: Job? = null
    private var telegramHealthJob: Job? = null
    private var signalRouteJob: Job? = null
    private var metaRouteJob: Job? = null
    private var youtubeRouteJob: Job? = null
    private var twitterRouteJob: Job? = null
    private var socialMediaJob: Job? = null
    private var lastPersistedState: String = ""
    private var lastNativeJournalAt: Long = 0L
    private var sessionBaseline: JSONObject = JSONObject()
    private val policyMemory by lazy { NetworkPolicyMemory(this) }
    private var policyMemoryKey: String? = null
    private val pendingProbes = ConcurrentHashMap.newKeySet<String>()
    private val exhaustedProbes = ConcurrentHashMap.newKeySet<String>()
    private val probeAttempts = ConcurrentHashMap<String, Int>()
    private val componentResults = ConcurrentHashMap<String, CheckResult>()
    private val socialRouteResults = ConcurrentHashMap<String, CheckResult>()
    private val socialMediaSamples = ConcurrentHashMap<String, CheckResult>()
    private var whatsappNativeRouteSeen = false
    private var lastNotificationSignature = ""

    override fun onCreate() {
        super.onCreate()
        getSystemService(NotificationManager::class.java).createNotificationChannel(
            NotificationChannel(CHANNEL, "Argus LIGHT", NotificationManager.IMPORTANCE_LOW),
        )
    }

    override fun onStartCommand(intent: Intent?, flags: Int, startId: Int): Int {
        if (BuildConfig.DEBUG && intent?.action == ACTION_VOICE_MAPPING_TRIAL) {
            val enabled = intent.getBooleanExtra("enabled", false)
            engine?.setVoiceMappingTrial(enabled)
            DiagnosticJournal.record("voice-trial", "stable UDP source mapping enabled=$enabled enginePresent=${engine != null}")
            return START_NOT_STICKY
        }
        if (BuildConfig.DEBUG && intent?.action == ACTION_SIGNAL_DIRECT_TRIAL) {
            val enabled = intent.getBooleanExtra("enabled", false)
            engine?.setSignalDirectTrial(enabled)
            DiagnosticJournal.record("signal-trial", "official direct trial enabled=$enabled enginePresent=${engine != null}")
            return START_NOT_STICKY
        }
        if (intent?.action == ACTION_STOP) {
            shutdown(intent.getStringExtra(EXTRA_REASON) ?: "LIGHT выключен", error = false)
            return START_NOT_STICKY
        }
        startForeground(NOTIFICATION_ID, notification(LightState(phase = LightPhase.STARTING)))
        if (engine == null) scope.launch { startLight() }
        return START_NOT_STICKY
    }

    override fun onBind(intent: Intent?): IBinder? = super.onBind(intent)

    override fun onRevoke() {
        shutdown("Системное разрешение VPN отозвано", error = true)
        super.onRevoke()
    }

    override fun onDestroy() {
        closeRuntime()
        scope.cancel()
        super.onDestroy()
    }

    private fun startLight() {
        runCatching {
            val physical = findPhysicalNetwork() ?: error("Нет рабочего Wi‑Fi или мобильного интернета")
            underlying = physical
            val metered = connectivity.getNetworkCapabilities(physical)
                ?.hasCapability(NetworkCapabilities.NET_CAPABILITY_NOT_METERED) == false
            val descriptor = Builder()
                .setSession("Argus LIGHT")
                .setMtu(MTU)
                .setBlocking(true)
                .setMetered(metered)
                .setUnderlyingNetworks(arrayOf(physical))
                .addAddress("10.111.0.2", 32)
                .addAddress("fd42:6172:6775::2", 128)
                .addRoute("0.0.0.0", 0)
                .addRoute("::", 0)
                .addDnsServer("10.111.0.1")
                .establish() ?: error("Android не создал VPN-интерфейс")
            tun = descriptor

            val datapath = Datapath.start(descriptor.fd.toLong(), MTU.toLong())
            engine = datapath
            datapath.setNetwork(physical.networkHandle)
            datapath.verifyBinding()
            val nat64Prefix = connectivity.getLinkProperties(physical)?.nat64Prefix?.toString().orEmpty()
            datapath.setNAT64Prefix(nat64Prefix)
            DiagnosticJournal.record("network", "NAT64 prefix available=${nat64Prefix.isNotEmpty()}")
            policyMemoryKey = policyMemory.key(physical)
            val remembered = policyMemory.load(policyMemoryKey)
            datapath.loadLightState(remembered)
            DiagnosticJournal.record("strategy-memory", "networkIdentified=${policyMemoryKey != null} restored=${remembered.isNotEmpty()}")
            val proxyPool = getSharedPreferences(PREFERENCES, Context.MODE_PRIVATE)
                .getString(KEY_PROXY_POOL, "").orEmpty()
            datapath.setProxyPool(proxyPool)
            datapath.setSignalRelay(getSharedPreferences(PREFERENCES, Context.MODE_PRIVATE)
                .getBoolean("signal_relay_sgpx_consent", false))
            datapath.setMTProxyDiscovery(getSharedPreferences(PREFERENCES, Context.MODE_PRIVATE)
                .getBoolean("mtproxy_public_discovery", false))
            DiagnosticJournal.record(
                "proxy",
                "SOCKS fallback pool loaded count=${proxyPool.lineSequence().count { it.isNotBlank() }}",
            )
            sessionBaseline = runCatching { JSONObject(datapath.lightStatus()) }.getOrElse { JSONObject() }
            datapath.setLightMode(true)
            setUnderlyingNetworks(arrayOf(physical))
            watchPhysicalNetwork(physical)

            controller.publish(
                LightState(
                    phase = LightPhase.VERIFYING,
                    title = "LIGHT исправляет соединение",
                    detail = "Запускаю отдельный подбор для пяти сервисов…",
                ),
            )
            launchSyntheticChecks()
            signalRouteJob?.cancel()
            val signalEngine = engine
            signalRouteJob = scope.launch {
                val ca = resources.openRawResource(R.raw.signal_ca).bufferedReader().use { it.readText() }
                while (true) {
                    currentCoroutineContext().ensureActive()
                    if (stopping.get() || engine !== signalEngine) break
                    for (host in listOf("grpc.chat.signal.org", "api.whatsapp.net", "chat.signal.org")) {
                        currentCoroutineContext().ensureActive()
                        if (stopping.get() || engine !== signalEngine) break
                        val evidence = signalEngine?.probeServiceRoute(host, ca).orEmpty()
                        currentCoroutineContext().ensureActive()
                        DiagnosticJournal.record("service-route-refresh", evidence)
                    }
                    delay(45_000)
                }
            }
            youtubeRouteJob?.cancel()
            youtubeRouteJob = scope.launch {
                while (true) {
                    for (host in listOf("www.youtube.com", "youtubei.googleapis.com", "youtube.googleapis.com")) {
                        currentCoroutineContext().ensureActive()
                        if (stopping.get() || engine !== signalEngine) return@launch
                        val evidence = signalEngine?.probeServiceRoute(host, "").orEmpty()
                        DiagnosticJournal.record("youtube-route-refresh", evidence)
                    }
                    delay(30_000)
                }
            }
            metaRouteJob?.cancel()
            metaRouteJob = scope.launch {
                while (true) {
                    currentCoroutineContext().ensureActive()
                    if (stopping.get() || engine !== signalEngine) break
                    for (host in listOf("www.instagram.com", "www.facebook.com", "scontent-hel3-1.xx.fbcdn.net", "scontent-arn2-1.xx.fbcdn.net")) {
                        currentCoroutineContext().ensureActive()
                        if (stopping.get() || engine !== signalEngine) break
                        val evidence = signalEngine?.probeServiceRoute(host, "").orEmpty()
                        DiagnosticJournal.record("meta-route-refresh", evidence)
                        recordSocialRoute(host, evidence)
                    }
                    delay(60_000)
                }
            }
            twitterRouteJob?.cancel()
            twitterRouteJob = scope.launch {
                while (true) {
                    for (host in listOf("x.com", "api.x.com", "api.twitter.com", "pbs.twimg.com", "video.twimg.com")) {
                        currentCoroutineContext().ensureActive()
                        if (stopping.get() || engine !== signalEngine) return@launch
                        val evidence = signalEngine?.probeServiceRoute(host, "").orEmpty()
                        DiagnosticJournal.record("twitter-route-refresh", evidence)
                        recordSocialRoute(host, evidence)
                    }
                    delay(60_000)
                }
            }
            watchStatus()
            watchTelegramHealth()
            socialMediaJob?.cancel()
            socialMediaJob = scope.launch {
                // Keep optional sample downloads away from cold-start playback.
                delay(AUTOTEST_DEADLINE_MS)
                for ((service, page) in listOf(
                    "facebook" to "https://www.facebook.com/messenger/videos/1811478386366704/",
                    "instagram" to "https://www.instagram.com/instagram/",
                )) {
                    currentCoroutineContext().ensureActive()
                    if (stopping.get() || engine !== signalEngine) break
                    val result = probeSocialVideoSample(service, page)
                    currentCoroutineContext().ensureActive()
                    if (engine === signalEngine) socialMediaSamples[service] = result
                }
            }
        }.onFailure { failure ->
            shutdown(readable(failure), error = true)
        }
    }

    private fun recordSocialRoute(host: String, evidence: String) {
        socialRouteResults[host] = if (runCatching { JSONObject(evidence).optBoolean("verified") }.getOrDefault(false)) CheckResult.ROUTE else CheckResult.FAILED
    }

    private fun socialRoute(vararg hosts: String): CheckResult {
        val results = hosts.map { socialRouteResults[it] ?: CheckResult.WAITING }
        return when {
            results.any { it == CheckResult.FAILED } -> CheckResult.FAILED
            results.all { it == CheckResult.ROUTE } -> CheckResult.ROUTE
            else -> CheckResult.WAITING
        }
    }

    private fun findPhysicalNetwork(): Network? {
        val candidates = connectivity.allNetworks.filter { network ->
            val caps = connectivity.getNetworkCapabilities(network) ?: return@filter false
            !caps.hasTransport(NetworkCapabilities.TRANSPORT_VPN) &&
                caps.hasCapability(NetworkCapabilities.NET_CAPABILITY_INTERNET) &&
                caps.hasCapability(NetworkCapabilities.NET_CAPABILITY_VALIDATED)
        }
        return candidates.firstOrNull { it == connectivity.activeNetwork }
            ?: candidates.firstOrNull { connectivity.getNetworkCapabilities(it)?.hasTransport(NetworkCapabilities.TRANSPORT_WIFI) == true }
            ?: candidates.firstOrNull()
    }

    private fun watchPhysicalNetwork(network: Network) {
        val request = NetworkRequest.Builder()
            .addCapability(NetworkCapabilities.NET_CAPABILITY_INTERNET)
            .build()
        val callback = object : ConnectivityManager.NetworkCallback() {
            override fun onLost(lost: Network) {
                if (lost == network) shutdown("Основная сеть потеряна — трафик закрыт", error = true)
            }
        }
        networkCallback = callback
        connectivity.registerNetworkCallback(request, callback)
    }

    private data class ProbeTarget(val url: String, val minimumBytes: Int = 0)
    private data class ProbeProfile(
        val id: String,
        val service: String,
        val targets: List<ProbeTarget>,
        val attemptBudget: Int = PROBE_STRATEGY_BUDGET,
    )

    private fun launchSyntheticChecks() {
        val profiles = listOf(
            // Telegram gets a real MTProto req_pq/resPQ probe in the native
            // engine; its documentation website is not message-path evidence.
            ProbeProfile("telegram", "telegram", emptyList()),
            ProbeProfile(
                "whatsapp-messages",
                "whatsapp",
                listOf(ProbeTarget("https://www.whatsapp.com/robots.txt")),
            ),
            ProbeProfile(
                "whatsapp-media-route",
                "whatsapp",
                listOf(
                    ProbeTarget("https://mmg.whatsapp.net/"),
                    ProbeTarget("https://static.whatsapp.net/robots.txt"),
                ),
            ),
            ProbeProfile(
                "youtube-feed",
                "youtube",
                emptyList(),
            ),
            ProbeProfile(
                "youtube-thumbnails",
                "youtube",
                listOf(ProbeTarget("https://i.ytimg.com/vi/jNQXAC9IVRw/hqdefault.jpg", minimumBytes = 5 * 1024)),
            ),
            ProbeProfile(
                "youtube-video-route",
                "youtube",
                listOf(ProbeTarget("https://redirector.googlevideo.com/report_mapping?di=no")),
            ),
            ProbeProfile("youtube-video-sample", "youtube", emptyList(), attemptBudget = 2),
            ProbeProfile("signal", "signal", listOf(ProbeTarget("https://chat.signal.org/"))),
            ProbeProfile("threema", "threema", listOf(ProbeTarget("https://ds-apip.threema.ch/"))),
        )
        pendingProbes.clear()
        exhaustedProbes.clear()
        probeAttempts.clear()
        componentResults.clear()
        socialRouteResults.clear()
        socialMediaSamples.clear()
        whatsappNativeRouteSeen = false
        lastNotificationSignature = ""
        pendingProbes.addAll(profiles.map { it.id })
        probeSuiteJob?.cancel()
        probeSuiteJob = scope.launch {
            // Give the TUN/netstack hand-off a brief deterministic settling
            // window before all service probes emit their first DNS burst.
            delay(PROBE_START_DELAY_MS)
            val jobs = profiles.map { profile -> launch { probeUntilWorking(profile) } }
            val completed = withTimeoutOrNull(AUTOTEST_DEADLINE_MS) {
                jobs.joinAll()
                true
            } ?: false
            if (!completed) {
                val timedOut = pendingProbes.toList()
                timedOut.forEach { id ->
                    componentResults.putIfAbsent(id, CheckResult.FAILED)
                    exhaustedProbes.add(id)
                    pendingProbes.remove(id)
                }
                DiagnosticJournal.record("autotest", "deadline reached pending=${timedOut.sorted()}")
                jobs.forEach { it.cancel() }
            }
        }
    }

    private suspend fun probeUntilWorking(profile: ProbeProfile) {
        val routeHosts = when (profile.id) {
            "signal" -> listOf("chat.signal.org")
            "whatsapp-messages" -> listOf("www.whatsapp.com")
            "whatsapp-media-route" -> listOf("mmg.whatsapp.net", "static.whatsapp.net")
            else -> emptyList()
        }
        for (host in routeHosts) {
            currentCoroutineContext().ensureActive()
            val ca = if (host == "chat.signal.org") resources.openRawResource(R.raw.signal_ca).bufferedReader().use { it.readText() } else ""
            val evidence = engine?.probeServiceRoute(host, ca).orEmpty()
            DiagnosticJournal.record("verified-service-route", evidence)
        }
        repeat(profile.attemptBudget) { index ->
            probeAttempts[profile.id] = index + 1
            val result = when (profile.id) {
                "telegram" -> if (engine?.probeTelegram() == true) CheckResult.ROUTE else CheckResult.FAILED
                "youtube-feed" -> probeYouTubeFeed()
                "youtube-video-sample" -> probeYouTubeVideoSample()
                else -> if (profile.targets.all { target -> probe(target.url, target.minimumBytes) }) {
                    // A public HTTP endpoint is route evidence, not proof that
                    // an authenticated messenger exchanged a message or file.
                    if (profile.id in setOf("whatsapp-messages", "signal", "threema")) CheckResult.ROUTE else CheckResult.WORKS
                } else {
                    CheckResult.FAILED
                }
            }
            currentCoroutineContext().ensureActive()
            if (result == CheckResult.WORKS || result == CheckResult.ROUTE) {
                componentResults[profile.id] = result
                pendingProbes.remove(profile.id)
                exhaustedProbes.remove(profile.id)
                return
            }
            delay(PROBE_RETRY_DELAY_MS)
        }
        componentResults[profile.id] = CheckResult.FAILED
        pendingProbes.remove(profile.id)
        exhaustedProbes.add(profile.id)
    }

    /**
     * Fetch the same browse surface the YouTube client needs for its home feed.
     * A homepage or generate_204 response alone is deliberately insufficient.
     */
    private fun probeYouTubeFeed(): CheckResult {
        return runCatching {
            val homepage = youtubeBootstrap() ?: return@runCatching CheckResult.FAILED
            val client = JSONObject()
                .put("clientName", "WEB")
                .put("clientVersion", homepage.second)
                .put("hl", "en")
                .put("gl", "US")
            val request = JSONObject()
                .put("context", JSONObject().put("client", client))
                .put("browseId", "FEwhat_to_watch")
                .toString().toByteArray()
            val response = httpBytes(
                target = "https://www.youtube.com/youtubei/v1/browse?key=${homepage.first}",
                method = "POST",
                request = request,
                headers = mapOf(
                    "Content-Type" to "application/json",
                    "X-YouTube-Client-Name" to "1",
                    "X-YouTube-Client-Version" to homepage.second,
                ),
                maxBytes = 1024 * 1024,
            )
            val body = response.second.toString(Charsets.UTF_8)
            val feedPresent = body.contains("\"videoId\"") &&
                (body.contains("videoRenderer") || body.contains("videoWithContextRenderer"))
            DiagnosticJournal.record(
                "youtube-feed-probe",
                "status=${response.first} bytes=${response.second.size} feed=$feedPresent",
            )
            if (response.first == 200 && response.second.size >= 20 * 1024 && feedPresent) {
                CheckResult.WORKS
            } else if (response.first == 200 && runCatching {
                    JSONObject(body).optJSONObject("responseContext") != null
                }.getOrDefault(false)) {
                // Logged-out YouTube may return an empty recommendations feed.
                // A valid API envelope proves reachability, not playback or a
                // populated feed. Do not keep retrying it as a network failure.
                CheckResult.ROUTE
            } else {
                CheckResult.FAILED
            }
        }.getOrElse { failure ->
            DiagnosticJournal.record("youtube-feed-probe", DiagnosticJournal.redact(readable(failure)))
            CheckResult.FAILED
        }
    }

    private fun youtubeBootstrap(): Pair<String, String>? {
        val homepage = httpBytes("https://www.youtube.com/", maxBytes = 512 * 1024)
        if (homepage.first !in 200..399 || homepage.second.size < 20 * 1024) return null
        val page = homepage.second.toString(Charsets.UTF_8)
        val key = Regex("\\\"INNERTUBE_API_KEY\\\":\\\"([^\\\"]+)\\\"")
            .find(page)?.groupValues?.get(1) ?: return null
        val version = Regex("\\\"INNERTUBE_CLIENT_VERSION\\\":\\\"([^\\\"]+)\\\"")
            .find(page)?.groupValues?.get(1) ?: return null
        return key to version
    }

    private fun probe(target: String, minimumBytes: Int): Boolean {
        var connection: HttpURLConnection? = null
        return runCatching {
            connection = URL(target).openConnection() as HttpURLConnection
            SignalTrust.configure(this, connection!!)
            connection!!.useCaches = false
            connection!!.setRequestProperty("Cache-Control", "no-cache, no-store, max-age=0")
            connection!!.setRequestProperty("Pragma", "no-cache")
            connection!!.setRequestProperty("Connection", "close")
            if (minimumBytes == 0) connection!!.setRequestProperty("Range", "bytes=0-0")
            connection!!.instanceFollowRedirects = false
            connection!!.connectTimeout = PROBE_TIMEOUT_MS
            connection!!.readTimeout = PROBE_TIMEOUT_MS
            connection!!.requestMethod = "GET"
            connection!!.connect()
            val code = connection!!.responseCode
            // Consume a bounded part of the actual response. A TCP/TLS header
            // alone is not media-path evidence and used to make YouTube green
            // even when the carrier throttled the body to an unusable rate.
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
            code in 100..599 && received >= minimumBytes
        }.getOrElse { failure ->
            DiagnosticJournal.record("service-probe", "host=${URL(target).host} error=${DiagnosticJournal.redact(readable(failure))}")
            false
        }.also {
            connection?.disconnect()
        }
    }

    /**
     * Resolve a fresh signed YouTube media URL and consume only a 512 KiB range.
     * A reachable player endpoint that refuses automation is ROUTE, never WORKS.
     */
    private fun probeYouTubeVideoSample(): CheckResult {
        return runCatching {
            val bootstrap = youtubeBootstrap() ?: return@runCatching CheckResult.FAILED
            val key = bootstrap.first
            val webVersion = bootstrap.second
            val clients = listOf(
                Triple("WEB", webVersion, "1"),
                Triple("ANDROID", "19.44.38", "3"),
                Triple("IOS", "19.45.4", "5"),
            )
            var playerAnswered = false
            for ((clientName, version, clientCode) in clients) {
                val client = JSONObject()
                    .put("clientName", clientName)
                    .put("clientVersion", version)
                    .put("hl", "en")
                    .put("gl", "US")
                if (clientName == "ANDROID") client.put("androidSdkVersion", 30)
                if (clientName == "IOS") {
                    client.put("deviceMake", "Apple")
                    client.put("deviceModel", "iPhone16,2")
                }
                val request = JSONObject()
                    .put("context", JSONObject().put("client", client))
                    .put("videoId", YOUTUBE_TEST_VIDEO)
                    .put("contentCheckOk", true)
                    .put("racyCheckOk", true)
                    .toString().toByteArray()
                val response = httpBytes(
                    target = "https://youtubei.googleapis.com/youtubei/v1/player?key=$key",
                    method = "POST",
                    request = request,
                    headers = mapOf(
                        "Content-Type" to "application/json",
                        "X-YouTube-Client-Name" to clientCode,
                        "X-YouTube-Client-Version" to version,
                    ),
                    maxBytes = 1024 * 1024,
                )
                if (response.first in 200..399 && response.second.size >= 4 * 1024) playerAnswered = true
                if (response.first != 200) continue
                val root = runCatching { JSONObject(response.second.toString(Charsets.UTF_8)) }.getOrNull()
                    ?: continue
                val formats = root.optJSONObject("streamingData")?.optJSONArray("adaptiveFormats")
                    ?: continue
                for (index in 0 until formats.length()) {
                    val item = formats.optJSONObject(index) ?: continue
                    if (!item.optString("mimeType").startsWith("video/")) continue
                    val mediaUrl = item.optString("url")
                    if (mediaUrl.isBlank()) continue
                    val media = httpBytes(
                        target = mediaUrl,
                        headers = mapOf(
                            "Range" to "bytes=0-524287",
                            "Origin" to "https://www.youtube.com",
                        ),
                        maxBytes = 512 * 1024,
                    )
                    if (media.first in listOf(200, 206) && media.second.size >= 256 * 1024 && MediaSampleCheck.isVideo(media.second)) {
                        DiagnosticJournal.record("youtube-video-probe", "real media bytes=${media.second.size}")
                        return@runCatching CheckResult.WORKS
                    }
                }
            }
            if (playerAnswered) CheckResult.ROUTE else CheckResult.FAILED
        }.getOrElse { failure ->
            DiagnosticJournal.record(
                "youtube-video-probe",
                DiagnosticJournal.redact(readable(failure)),
            )
            CheckResult.FAILED
        }
    }

    private fun probeSocialVideoSample(service: String, page: String): CheckResult = runCatching {
        val response = httpBytes(page, maxBytes = 1024 * 1024)
        if (response.first !in 200..399) return@runCatching CheckResult.WAITING
        val document = response.second.toString(Charsets.UTF_8).replace("&quot;", "\"").replace("&#34;", "\"").replace("&amp;", "&")
        val links = Regex(""""(?:browser_native_sd_url|browser_native_hd_url|playable_url|video_url|contentUrl)"\s*:\s*("(?:[^"\\]|\\.)*")""")
            .findAll(document).mapNotNull { match ->
                runCatching { org.json.JSONTokener(match.groupValues[1]).nextValue() as? String }.getOrNull()
            }.filter { link ->
                runCatching {
                    val url = URL(link)
                    url.protocol == "https" && (url.host.endsWith(".fbcdn.net") || url.host.endsWith(".cdninstagram.com"))
                }.getOrDefault(false)
            }.distinct().take(2).toList()
        for (link in links) {
            val sample = httpBytes(link, headers = mapOf("Range" to "bytes=0-262143"), maxBytes = 256 * 1024)
            if (sample.first in listOf(200,206) && sample.second.size >= 128 * 1024 && MediaSampleCheck.isVideo(sample.second)) {
                DiagnosticJournal.record("social-video-probe", "$service fresh-video-container bytes=${sample.second.size} playback=not-tested")
                return@runCatching CheckResult.WORKS
            }
        }
        DiagnosticJournal.record("social-video-probe", "$service sample-unavailable candidates=${links.size} pageHTTP=${response.first}; not proof of blocked playback")
        CheckResult.WAITING
    }.getOrElse {
        DiagnosticJournal.record("social-video-probe", "$service sample-unavailable; ${DiagnosticJournal.redact(readable(it))}")
        CheckResult.WAITING
    }

    private fun httpBytes(
        target: String,
        method: String = "GET",
        request: ByteArray? = null,
        headers: Map<String, String> = emptyMap(),
        maxBytes: Int,
    ): Pair<Int, ByteArray> {
        var connection: HttpURLConnection? = null
        return try {
            connection = URL(target).openConnection() as HttpURLConnection
            connection.useCaches = false
            connection.setRequestProperty("Cache-Control", "no-cache, no-store, max-age=0")
            connection.setRequestProperty("Pragma", "no-cache")
            connection.instanceFollowRedirects = true
            connection.connectTimeout = PROBE_TIMEOUT_MS
            connection.readTimeout = PROBE_TIMEOUT_MS
            connection.requestMethod = method
            connection.setRequestProperty("Connection", "close")
            connection.setRequestProperty("User-Agent", YOUTUBE_USER_AGENT)
            headers.forEach(connection::setRequestProperty)
            if (request != null) {
                connection.doOutput = true
                connection.outputStream.use { it.write(request) }
            }
            val code = connection.responseCode
            val source = if (code >= 400) connection.errorStream else connection.inputStream
            val bytes = ByteArrayOutputStream(minOf(maxBytes, 64 * 1024))
            source?.use { input ->
                val buffer = ByteArray(8 * 1024)
                while (bytes.size() < maxBytes) {
                    val count = input.read(buffer, 0, minOf(buffer.size, maxBytes - bytes.size()))
                    if (count < 0) break
                    bytes.write(buffer, 0, count)
                }
            }
            code to bytes.toByteArray()
        } finally {
            connection?.disconnect()
        }
    }

    private fun watchTelegramHealth() {
        telegramHealthJob?.cancel()
        telegramHealthJob = scope.launch {
            while (true) {
                delay(30_000)
                if (pendingProbes.contains("telegram")) continue
                val ok = runCatching { engine?.probeTelegram() == true }.getOrDefault(false)
                currentCoroutineContext().ensureActive()
                componentResults["telegram"] = if (ok) CheckResult.ROUTE else CheckResult.FAILED
                if (ok) exhaustedProbes.remove("telegram") else exhaustedProbes.add("telegram")
                DiagnosticJournal.record("telegram-health", "current MTProto nonce verified=$ok")
            }
        }
    }

    private fun watchStatus() {
        statusJob?.cancel()
        statusJob = scope.launch {
            while (true) {
                delay(2_000)
                val encoded = engine?.lightStatus().orEmpty()
                val snapshot = runCatching { JSONObject(encoded) }.getOrNull() ?: continue
                controller.recordNativeStatus(encoded)
                if (encoded != lastPersistedState) {
                    val now = android.os.SystemClock.elapsedRealtime()
                    if (now - lastNativeJournalAt >= NATIVE_JOURNAL_INTERVAL_MS) {
                        DiagnosticJournal.record("native", encoded)
                        lastNativeJournalAt = now
                    }
                    saveStrategyMemory()
                    lastPersistedState = encoded
                }
                val next = stateFrom(snapshot)
                controller.publish(next)
                val signature = ServiceNotification.signature(next)
                if (signature != lastNotificationSignature) {
                    getSystemService(NotificationManager::class.java).notify(NOTIFICATION_ID, notification(next))
                    lastNotificationSignature = signature
                }
            }
        }
    }

    private fun stateFrom(root: JSONObject): LightState {
        val strategies = root.optJSONObject("strategies")
        val udp = root.optJSONObject("udp")
        val connect = root.optJSONObject("connect")
        val traffic = root.optJSONObject("traffic")
        val current = root.optJSONObject("current")
        fun tcp(service: String): CheckResult {
            val values = strategies?.optJSONObject(service)
            var attempts = 0
            var success = 0
            if (values != null) {
                for (key in values.keys()) {
                    val item = values.optJSONObject(key) ?: continue
                    attempts += item.optInt("attempts")
                    success += item.optInt("success")
                }
            }
            attempts -= strategyTotal(sessionBaseline, service, "attempts")
            success -= strategyTotal(sessionBaseline, service, "success")
            val route = connect?.optJSONObject(service)
            val baseRoute = sessionBaseline.optJSONObject("connect")?.optJSONObject(service)
            val routeAttempts = (route?.optInt("attempts") ?: 0) - (baseRoute?.optInt("attempts") ?: 0)
            val connected = (route?.optInt("connected") ?: 0) - (baseRoute?.optInt("connected") ?: 0)
            return when {
                success > 0 -> CheckResult.WORKS
                exhaustedProbes.contains(service) -> CheckResult.FAILED
                attempts > 0 || (routeAttempts > 0 && connected == 0) -> CheckResult.WAITING
                else -> CheckResult.WAITING
            }
        }
        fun media(service: String): CheckResult {
            val value = udp?.optJSONObject(service) ?: return CheckResult.WAITING
            val baseline = sessionBaseline.optJSONObject("udp")?.optJSONObject(service)
            val replies = value.optInt("replies") - (baseline?.optInt("replies") ?: 0)
            val attempts = value.optInt("attempts") - (baseline?.optInt("attempts") ?: 0)
            return when {
                replies > 0 -> CheckResult.ROUTE
                attempts > 0 && value.optBoolean("tcpFallback") -> CheckResult.FALLBACK
                attempts > 0 -> CheckResult.FAILED
                else -> CheckResult.WAITING
            }
        }
        fun component(id: String): CheckResult = componentResults[id]
            ?: if (exhaustedProbes.contains(id)) CheckResult.FAILED else CheckResult.WAITING
        fun transferredBytes(service: String): Long {
            val now = traffic?.optJSONObject(service)?.optLong("bytes") ?: 0L
            val before = sessionBaseline.optJSONObject("traffic")
                ?.optJSONObject(service)?.optLong("bytes") ?: 0L
            return (now - before).coerceAtLeast(0L)
        }
        val tgPool = root.optJSONObject("mtproxies")
        val tgStage = tgPool?.optString("stage").orEmpty()
        val tgPoolEnabled = (tgPool?.optInt("count") ?: 0) > 0 || tgPool?.optBoolean("auto") == true
        val tgRouteLost = tgPoolEnabled && tgStage in setOf("route-failed", "search-exhausted", "list-unavailable")
        val tgResult = when {
            // A failed spare-proxy search is not failure of every existing DC.
            // Fresh nonce evidence means only ROUTE, never working messages.
            tgPoolEnabled && (tgPool?.optJSONArray("verifiedMainDcs")?.length() ?: 0) > 0 -> CheckResult.ROUTE
            tgRouteLost -> CheckResult.FAILED
            tgPool?.optBoolean("searching") == true && component("telegram") !in setOf(CheckResult.WORKS, CheckResult.ROUTE) -> CheckResult.WAITING
            else -> component("telegram")
        }
        val telegram = ServiceCheck(tgResult, media("telegram"))
        val nativeWhatsApp = connect?.optJSONObject("whatsapp")
        if (nativeWhatsApp?.optString("route")?.startsWith("whatsapp-native-web/") == true && nativeWhatsApp.optInt("connected") > 0) {
            whatsappNativeRouteSeen = true
        }
        // The website probe is not the native Noise transport. A verified
        // native WebSocket connection is ROUTE evidence, never message delivery.
        val whatsappMessages = if (whatsappNativeRouteSeen) CheckResult.ROUTE else component("whatsapp-messages")
        val whatsapp = ServiceCheck(whatsappMessages, media("whatsapp"))
        val whatsappMediaRoute = component("whatsapp-media-route")
        val whatsappMedia = when {
            whatsappMediaRoute == CheckResult.FAILED -> CheckResult.FAILED
            whatsappMediaRoute == CheckResult.WORKS -> CheckResult.ROUTE
            else -> CheckResult.WAITING
        }
        val signal = ServiceCheck(component("signal"), media("signal"))
        val threema = ServiceCheck(component("threema"), media("threema"))
        val youtubeFeed = component("youtube-feed")
        val youtubeThumbnails = component("youtube-thumbnails")
        val youtubeVideoRoute = component("youtube-video-route")
        val youtubeVideoSample = component("youtube-video-sample")
        val youtubeVideo = when {
            youtubeVideoSample == CheckResult.WORKS -> CheckResult.WORKS
            youtubeVideoSample == CheckResult.ROUTE || youtubeVideoRoute == CheckResult.WORKS -> CheckResult.ROUTE
            youtubeVideoSample == CheckResult.FAILED && youtubeVideoRoute == CheckResult.FAILED -> CheckResult.FAILED
            else -> CheckResult.WAITING
        }
        val youtubeAutomaticDone = listOf(
            "youtube-feed",
            "youtube-thumbnails",
            "youtube-video-route",
            "youtube-video-sample",
        ).none(pendingProbes::contains)
        val youtube = when {
            youtubeFeed == CheckResult.FAILED || youtubeThumbnails == CheckResult.FAILED ||
                youtubeVideo == CheckResult.FAILED -> CheckResult.FAILED
            youtubeFeed == CheckResult.WORKS && youtubeThumbnails == CheckResult.WORKS &&
                youtubeVideo == CheckResult.WORKS -> CheckResult.WORKS
            youtubeAutomaticDone -> CheckResult.ROUTE
            else -> CheckResult.WAITING
        }
        val dns = root.optBoolean("doh")
        val nat64 = root.optBoolean("nat64")
        val telegramWss = root.optBoolean("telegramWss")
        val proxyCount = root.optJSONObject("proxies")?.optInt("count") ?: 0
        val mtproxy = root.optJSONObject("mtproxies")
        val mtproxyCount = mtproxy?.optInt("count") ?: 0
        val mtproxySearching = mtproxy?.optBoolean("searching") == true
        val recoveryFinished = pendingProbes.isEmpty()
        val dnsFailed = recoveryFinished &&
            root.optLong("dohAttempts") > 0 && root.optLong("dohSuccess") == 0L
        val serviceResults = listOf(
            telegram.messages, whatsapp.messages, signal.messages, threema.messages, youtube,
        )
        val failed = dnsFailed || serviceResults.any { it == CheckResult.FAILED }
        val proven = serviceResults.count { it == CheckResult.WORKS }
        val recovering = pendingProbes.toSet()
        val labels = mapOf(
            "telegram" to "Telegram",
            "whatsapp" to "WhatsApp",
            "whatsapp-messages" to "WhatsApp сообщения",
            "whatsapp-media-route" to "WhatsApp медиа",
            "signal" to "Signal",
            "threema" to "Threema",
            "youtube" to "YouTube",
            "youtube-feed" to "YouTube лента",
            "youtube-thumbnails" to "YouTube обложки",
            "youtube-video-route" to "YouTube видео",
            "youtube-video-sample" to "YouTube тестовый ролик",
        )
        fun failureReason(service: String, result: CheckResult): String? {
            if (result != CheckResult.FAILED) return null
            val route = connect?.optJSONObject(service)
            val baseline = sessionBaseline.optJSONObject("connect")?.optJSONObject(service)
            val attempts = (route?.optInt("attempts") ?: 0) - (baseline?.optInt("attempts") ?: 0)
            val connected = (route?.optInt("connected") ?: 0) - (baseline?.optInt("connected") ?: 0)
            val routes = root.optJSONObject("routes")?.optJSONObject(service)
            val ipv6Tried = routes?.keys()?.asSequence()?.any { route ->
                route.contains("ipv6") && (routes.optJSONObject(route)?.optInt("attempts") ?: 0) > 0
            } == true
            return if (attempts > 0 && connected == 0) {
                if (ipv6Tried) "Проверенные IPv4/IPv6 маршруты недоступны" else "Проверенные маршруты не ответили"
            } else {
                "Прикладная проверка не завершилась успешно"
            }
        }
        val resultByService = mapOf(
            "telegram" to telegram.messages,
            "whatsapp" to whatsapp.messages,
            "signal" to signal.messages,
            "threema" to threema.messages,
            "youtube" to youtube,
        )
        val failureReasons = resultByService.mapNotNull { (service, result) ->
            failureReason(service, result)?.let { service to it }
        }.toMap()
        val causes = buildList {
            if (dnsFailed) add("DNS не отвечает")
            failureReasons.forEach { (service, reason) -> add("${labels[service]}: $reason") }
            if (whatsappMedia == CheckResult.FAILED) add("WhatsApp медиа: CDN не отвечает")
            if (youtubeFeed == CheckResult.FAILED) add("YouTube: лента не загружается")
            if (youtubeThumbnails == CheckResult.FAILED) add("YouTube: обложки не загружаются")
            if (youtubeVideo == CheckResult.FAILED) add("YouTube: медиамаршрут не отвечает")
            if (youtubeVideo == CheckResult.ROUTE && youtubeAutomaticDone) {
                add("YouTube: маршрут есть, но тестовое видео не передало 256 КБ")
            }
            if (telegram.calls == CheckResult.FALLBACK) add("Telegram UDP→TCP")
            if (whatsapp.calls == CheckResult.FALLBACK) add("WhatsApp UDP→TCP")
        }
        val phase = when {
            recovering.isNotEmpty() -> LightPhase.VERIFYING
            failed -> LightPhase.LIMITED
            proven == 5 && dns -> LightPhase.ACTIVE
            else -> LightPhase.VERIFYING
        }
        return LightState(
            phase = phase,
            title = when (phase) {
                LightPhase.ACTIVE -> "Автотест завершён"
                LightPhase.LIMITED -> "Автотест завершён: есть проблемы"
                else -> "LIGHT исправляет соединение"
            },
            detail = when (phase) {
                LightPhase.ACTIVE -> "Проверена лента, обложки и тестовое видео • звонки подтверждаются реальным звонком"
                LightPhase.LIMITED -> "Проверки закончены • исправлено $proven/5 • ${causes.joinToString(" • ").ifEmpty { "нужен TUNNEL" }}"
                else -> {
                    val attempt = recovering.mapNotNull(probeAttempts::get).maxOrNull() ?: 1
                    val activeLabels = recovering.mapNotNull(labels::get).joinToString(", ")
                    "Исправлено $proven/5 • попытка $attempt/$PROBE_STRATEGY_BUDGET • сейчас: $activeLabels"
                }
            },
            telegram = telegram,
            whatsapp = whatsapp,
            whatsappMedia = whatsappMedia,
            facebook = SocialCheck(socialRoute("www.facebook.com"), socialMediaSamples["facebook"]?.takeIf { it == CheckResult.WORKS } ?: socialRoute("scontent-hel3-1.xx.fbcdn.net", "scontent-arn2-1.xx.fbcdn.net")),
            instagram = SocialCheck(socialRoute("www.instagram.com"), socialMediaSamples["instagram"] ?: CheckResult.WAITING),
            twitter = SocialCheck(socialRoute("x.com", "api.x.com", "api.twitter.com"), socialRoute("pbs.twimg.com", "video.twimg.com")),
            signal = signal,
            threema = threema,
            youtube = youtube,
            youtubeFeed = youtubeFeed,
            youtubeThumbnails = youtubeThumbnails,
            youtubeVideo = youtubeVideo,
            automaticChecksComplete = recoveryFinished,
            dohReady = dns,
            dnsTransport = root.optString("dnsTransport", "CHECK"),
            strategySummary = "Маршруты по сервисам • DNS IPv4/IPv6 • " +
                "Telegram MTProxy $mtproxyCount${if (mtproxySearching) " — поиск" else ""} • " +
                "WSS ${if (telegramWss) "активен" else "резерв"} • " +
                "NAT64 ${if (nat64) "активен" else "нет в сети"} • SOCKS $proxyCount • adaptive QUIC",
            probeAttempts = probeAttempts.toMap(),
            recoveringServices = recovering,
            currentStrategies = labels.keys.associateWith { id ->
                val service = id.substringBefore('-')
                current?.optString(service).orEmpty()
            },
            currentRoutes = labels.keys.associateWith { id ->
                val service = id.substringBefore('-')
                udp?.optJSONObject(service)?.optString("route").orEmpty().ifEmpty {
                    connect?.optJSONObject(service)?.optString("route").orEmpty()
                }
            },
            throughputKbps = labels.keys.associateWith { id ->
                val service = id.substringBefore('-')
                traffic?.optJSONObject(service)?.optLong("lastKbps") ?: 0L
            },
            failureReasons = failureReasons,
        )
    }

    private fun strategyTotal(root: JSONObject, service: String, field: String): Int {
        val values = root.optJSONObject("strategies")?.optJSONObject(service) ?: return 0
        var total = 0
        for (key in values.keys()) total += values.optJSONObject(key)?.optInt(field) ?: 0
        return total
    }

    private fun shutdown(reason: String, error: Boolean) {
        if (!stopping.compareAndSet(false, true)) return
        closeRuntime()
        controller.publish(
            if (error) LightState(LightPhase.ERROR, "LIGHT остановлен безопасно", reason)
            else LightState(detail = reason),
        )
        stopForeground(STOP_FOREGROUND_REMOVE)
        stopSelf()
    }

    private fun closeRuntime() {
        saveStrategyMemory()
        socialMediaJob?.cancel()
        socialMediaJob = null
        twitterRouteJob?.cancel()
        twitterRouteJob = null
        signalRouteJob?.cancel()
        signalRouteJob = null
        metaRouteJob?.cancel()
        metaRouteJob = null
        youtubeRouteJob?.cancel()
        youtubeRouteJob = null
        statusJob?.cancel()
        statusJob = null
        probeSuiteJob?.cancel()
        probeSuiteJob = null
        telegramHealthJob?.cancel()
        telegramHealthJob = null
        networkCallback?.let { runCatching { connectivity.unregisterNetworkCallback(it) } }
        networkCallback = null
        runCatching { engine?.stop() }
        engine = null
        runCatching { tun?.close() }
        tun = null
        underlying = null
        policyMemoryKey = null
        controller.recordNativeStatus("{\"enabled\":false}")
    }

    private fun saveStrategyMemory() {
        val network = underlying ?: return
        if (policyMemory.key(network) != policyMemoryKey) return
        val snapshot = runCatching { engine?.strategyMemory() }.getOrNull().orEmpty()
        policyMemory.save(policyMemoryKey, snapshot)
    }

    private fun notification(state: LightState): Notification {
        val open = PendingIntent.getActivity(
            this, 10, Intent(this, MainActivity::class.java),
            PendingIntent.FLAG_IMMUTABLE or PendingIntent.FLAG_UPDATE_CURRENT,
        )
        val stop = PendingIntent.getService(
            this, 11, Intent(this, ArgusLightService::class.java).setAction(ACTION_STOP),
            PendingIntent.FLAG_IMMUTABLE or PendingIntent.FLAG_UPDATE_CURRENT,
        )
        return Notification.Builder(this, CHANNEL)
            .setSmallIcon(R.drawable.ic_argus_status)
            .setContentTitle("Argus LIGHT")
            .setContentText(ServiceNotification.summary(state))
            .setSubText(if (state.automaticChecksComplete) "Проверки завершены" else "Проверяю соединения")
            .setStyle(Notification.DecoratedCustomViewStyle())
            .setCustomContentView(ServiceNotification.views(this, state, false))
            .setCustomBigContentView(ServiceNotification.views(this, state, true))
            .setOnlyAlertOnce(true)
            .setShowWhen(false)
            .setContentIntent(open)
            .setOngoing(true)
            .addAction(Notification.Action.Builder(null, "Выключить", stop).build())
            .build()
    }

    private fun readable(failure: Throwable): String {
        var root = failure
        while (root.cause != null && root.cause !== root) root = root.cause!!
        return root.message?.take(240) ?: root.javaClass.simpleName
    }

    companion object {
        const val ACTION_STOP = "com.argus.connect.STOP_LIGHT"
        const val ACTION_VOICE_MAPPING_TRIAL = "com.argus.connect.VOICE_MAPPING_TRIAL"
        const val ACTION_SIGNAL_DIRECT_TRIAL = "com.argus.connect.SIGNAL_DIRECT_TRIAL"
        const val EXTRA_REASON = "reason"
        private const val CHANNEL = "argus_light"
        private const val NOTIFICATION_ID = 4201
        private const val MTU = 1500
        private const val PREFERENCES = "argus_light"
        private const val KEY_PROXY_POOL = "proxy_pool"
        private const val PROBE_STRATEGY_BUDGET = 10
        private const val PROBE_TIMEOUT_MS = 5_000
        private const val PROBE_RETRY_DELAY_MS = 350L
        private const val PROBE_START_DELAY_MS = 750L
        private const val AUTOTEST_DEADLINE_MS = 45_000L
        private const val NATIVE_JOURNAL_INTERVAL_MS = 15_000L
        private const val YOUTUBE_TEST_VIDEO = "jNQXAC9IVRw"
        private const val YOUTUBE_USER_AGENT =
            "Mozilla/5.0 (Linux; Android 17; SM-F971B) AppleWebKit/537.36 Chrome/126 Mobile Safari/537.36"
    }
}
