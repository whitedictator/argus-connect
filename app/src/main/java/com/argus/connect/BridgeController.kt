package com.argus.connect

import android.content.Context
import android.content.Intent
import android.net.ConnectivityManager
import android.net.Network
import android.net.NetworkCapabilities
import android.net.NetworkRequest
import android.net.nsd.NsdManager
import android.net.nsd.NsdServiceInfo
import android.net.wifi.WifiManager
import android.os.Build
import com.flyfishxu.kadb.Kadb
import com.flyfishxu.kadb.cert.KadbCert
import datapath.Datapath
import datapath.Engine
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.Job
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.cancel
import kotlinx.coroutines.delay
import kotlinx.coroutines.launch
import kotlinx.coroutines.runBlocking
import kotlinx.coroutines.withContext
import java.net.NetworkInterface
import java.util.concurrent.CountDownLatch
import java.util.concurrent.CopyOnWriteArraySet
import java.util.concurrent.TimeUnit
import java.util.concurrent.atomic.AtomicReference

class BridgeController(private val context: Context) {
    private val scope = CoroutineScope(SupervisorJob() + Dispatchers.Main.immediate)
    private val listeners = CopyOnWriteArraySet<(BridgeState) -> Unit>()
    private val preferences = context.getSharedPreferences("argus", Context.MODE_PRIVATE)
    private val connectivity = context.getSystemService(ConnectivityManager::class.java)
    private val hasStoredIdentity = runCatching { KadbCert.exportIdentityOrNull() != null }.getOrDefault(false)

    @Volatile private var state = if (preferences.getBoolean(KEY_PAIRED, false) || hasStoredIdentity) {
        BridgeState(BridgePhase.STARTING, "Восстанавливаю bridge", "Проверяю сохранённую системную пару…")
    } else {
        BridgeState()
    }
    private var operation: Job? = null
    private var helper: ShellBridgeConnection? = null
    private var tun: android.os.ParcelFileDescriptor? = null
    private var datapath: Engine? = null
    private var vpnCallback: ConnectivityManager.NetworkCallback? = null
    private var vpnNetwork: Network? = null
    private var watchdog: Job? = null

    init {
        scope.launch(Dispatchers.IO) { retireLegacyHelper() }
        when {
            preferences.getBoolean(KEY_PAIRED, false) -> restoreSavedPairing()
            hasStoredIdentity -> recoverExistingPairing()
        }
    }

    private fun retireLegacyHelper() {
        val prefix = "argus_${context.packageName.replace('.', '_')}_resident_v"
        for (version in 1 until HELPER_VERSION) {
            runCatching {
                ShellBridgeConnection.connectResident("$prefix$version")?.use {
                    it.command("SHUTDOWN")
                }
            }
        }
    }

    fun current(): BridgeState = state

    fun hasSavedPairing(): Boolean = preferences.getBoolean(KEY_PAIRED, false)

    fun observe(listener: (BridgeState) -> Unit): AutoCloseable {
        listeners += listener
        listener(state)
        return AutoCloseable { listeners -= listener }
    }

    fun pair(code: String) {
        if (operation?.isActive == true) return
        update(BridgeState(BridgePhase.STARTING, "Подключение", "Ищу системное окно с 6-значным кодом…"))
        operation = scope.launch {
            val pairing = runCatching {
                val endpoint = withContext(Dispatchers.IO) {
                    discoverNsdEndpoint("_adb-tls-pairing._tcp", 12, "Окно сопряжения ADB не найдено")
                }
                DiagnosticJournal.record("bridge-stage", "pairing endpoint discovered port=${endpoint.second}")
                pairAndRemember(endpoint.first, endpoint.second, code)
            }
            val pairingFailure = pairing.exceptionOrNull()
            if (pairingFailure != null) {
                // Android can already hold the certificate even when an older
                // Argus build failed before saving its local flag. Prove that by
                // connecting with the existing identity before asking for a code.
                val recovered = runCatching {
                    withContext(Dispatchers.IO) { connectHelper().close() }
                }.isSuccess
                if (recovered) {
                    preferences.edit()
                        .putBoolean(KEY_PAIRED, true)
                        .putInt(KEY_HELPER_VERSION, HELPER_VERSION)
                        .apply()
                    DiagnosticJournal.record("bridge-stage", "pair request replaced by existing Android pairing")
                } else {
                    val reason = readable(pairingFailure)
                    DiagnosticJournal.record("bridge-error", "pair/helper stage: ${DiagnosticJournal.redact(reason)}")
                    if (hasSavedPairing()) {
                        update(
                            BridgeState(
                                BridgePhase.SETUP_REQUIRED,
                                "Пара сохранена, bridge не запущен",
                                "$reason. Оставьте беспроводную отладку включённой и нажмите восстановить.",
                            ),
                        )
                    } else {
                        fail("Настройка не завершена", reason)
                    }
                    return@launch
                }
            }

            if (findVpn() == null) {
                update(BridgeState(BridgePhase.READY, "Bridge готов", "Включите VPN, затем нажмите hotspot"))
                // The resident shell process is deliberately owned by a live
                // local-ADB session. Keep our app process foreground while the
                // prepared bridge is waiting for the user to enable a VPN.
                BridgeRuntimeService.start(context)
                return@launch
            }
            update(BridgeState(BridgePhase.READY, "Bridge готов", "Пара сохранена. Нажмите значок раздачи, чтобы включить hotspot"))
            BridgeRuntimeService.start(context)
        }
    }

    fun toggle() {
        when {
            state.isBusy -> Unit
            state.isActive -> stop("Остановлено")
            !preferences.getBoolean(KEY_PAIRED, false) -> update(BridgeState())
            else -> start()
        }
    }

    fun clearPairing() {
        stop("Настройка сброшена")
        runCatching {
            ShellBridgeConnection.connectResident(residentSocketName())?.use {
                it.command("SHUTDOWN")
            }
        }
        runCatching { KadbCert.clear() }
        preferences.edit().clear().apply()
        update(BridgeState())
    }

    fun start() {
        if (operation?.isActive == true || state.isActive) return
        update(BridgeState(BridgePhase.STARTING, "1/5 · Проверяю VPN", "Ищу включённый VPN для раздачи…"))
        operation = scope.launch {
            runCatching { startProtectedHotspot() }
                .onFailure { failure ->
                    stopInternal()
                    val reason = bridgeFailure(failure)
                    DiagnosticJournal.record("bridge-error", "hotspot stage: ${DiagnosticJournal.redact(reason)}")
                    fail("Hotspot не запущен", reason)
                }
        }
    }

    fun restoreSavedPairing() {
        if (operation?.isActive == true) return
        update(BridgeState(BridgePhase.STARTING, "Восстанавливаю bridge", "Ищу сохранённую системную пару…"))
        operation = scope.launch {
            runCatching {
                withContext(Dispatchers.IO) { connectHelper().close() }
            }.onSuccess {
                preferences.edit().putInt(KEY_HELPER_VERSION, HELPER_VERSION).apply()
                update(BridgeState(BridgePhase.READY, "Bridge готов", "Сохранённая пара восстановлена без нового кода"))
                BridgeRuntimeService.start(context)
            }.onFailure { failure ->
                val reason = bridgeFailure(failure)
                DiagnosticJournal.record(
                    "bridge-error",
                    "restore helper: ${DiagnosticJournal.redact(reason)}",
                )
                update(
                    BridgeState(
                        BridgePhase.SETUP_REQUIRED,
                        "Нужно восстановить bridge",
                        "$reason. Новый код нужен только если Android забыл пару",
                    ),
                )
            }
        }
    }

    private fun recoverExistingPairing() {
        if (operation?.isActive == true) return
        update(BridgeState(BridgePhase.STARTING, "Проверяю системную пару", "Новый код пока не нужен…"))
        operation = scope.launch {
            runCatching {
                withContext(Dispatchers.IO) { connectHelper().close() }
            }.onSuccess {
                preferences.edit()
                    .putBoolean(KEY_PAIRED, true)
                    .putInt(KEY_HELPER_VERSION, HELPER_VERSION)
                    .apply()
                DiagnosticJournal.record("bridge-stage", "existing Android pairing recovered")
                update(BridgeState(BridgePhase.READY, "Bridge готов", "Системная пара найдена без нового кода"))
                BridgeRuntimeService.start(context)
            }.onFailure { failure ->
                DiagnosticJournal.record(
                    "bridge-stage",
                    "stored identity not accepted: ${DiagnosticJournal.redact(readable(failure))}",
                )
                update(BridgeState())
            }
        }
    }

    fun stop(reason: String = "Остановлено") {
        if (operation?.isActive == true) operation?.cancel()
        update(state.copy(phase = BridgePhase.STOPPING, title = "Остановка", detail = "Закрываю hotspot…"))
        operation = scope.launch {
            withContext(Dispatchers.IO) { stopInternal() }
            update(BridgeState(BridgePhase.READY, "Готов", reason))
            BridgeRuntimeService.stop(context)
        }
    }

    private suspend fun pairAndRemember(hostHint: String, port: Int, code: String) = withContext(Dispatchers.IO) {
        require(port in 1024..65535) { "Порт должен быть от 1024 до 65535" }
        require(code.matches(Regex("\\d{6}"))) { "Нужен 6-значный код" }

        val candidates = linkedSetOf(hostHint, "127.0.0.1", "::1")
        NetworkInterface.getNetworkInterfaces().toList().flatMap { it.inetAddresses.toList() }
            .filterNot { it.isLoopbackAddress || it.isLinkLocalAddress }
            .mapNotNull { it.hostAddress?.substringBefore('%') }
            .forEach { candidates += it }

        var last: Throwable? = null
        for (host in candidates) {
            val result = runCatching { runBlocking { Kadb.pair(host, port, code, "Argus Connect") } }
            if (result.isSuccess) {
                // Pairing belongs to Android and remains valid independently
                // from the following connect/helper step. Persist it now so a
                // temporary connect-port failure never asks for a second code.
                preferences.edit().putBoolean(KEY_PAIRED, true).apply()
                DiagnosticJournal.record("bridge-stage", "Android pairing accepted")
                // Start the guarded shell helper while Wireless Debugging is
                // still reachable. It can then serve a later mobile-data run.
                val resident = connectHelper()
                resident.close()
                preferences.edit()
                    .putInt(KEY_HELPER_VERSION, HELPER_VERSION)
                    .apply()
                DiagnosticJournal.record("bridge-stage", "resident helper reachable")
                return@withContext
            }
            last = result.exceptionOrNull()
        }
        throw IllegalStateException("pairing отклонён: ${last?.message}", last)
    }

    private suspend fun startProtectedHotspot() = withContext(Dispatchers.IO) {
        val vpn = findVpn() ?: error("Включите VPN: WireGuard, Happ или LIGHT Argus. VPN должен разрешать трафик Argus")
        vpnNetwork = vpn
        DiagnosticJournal.record("bridge-stage", "VPN network selected")

        progress("2/5 · Подключаю системный мост", "Проверяю сохранённый доступ Android…")
        val bridge = connectHelper()
        helper = bridge
        DiagnosticJournal.record("bridge-stage", "resident helper connected")
        BridgeRuntimeService.start(context)
        val probe = bridge.command("PROBE")
        check(probe.startsWith("OK|")) { probe.removePrefix("ERROR|") }
        DiagnosticJournal.record("bridge-stage", "shell capability probe accepted")

        progress("3/5 · Готовлю защищённый маршрут", "Создаю отдельный сетевой интерфейс для клиентов…")
        val prepared = bridge.prepare()
        tun = prepared.descriptor
        DiagnosticJournal.record("bridge-stage", "test TUN prepared")
        val engine = Datapath.start(prepared.descriptor.fd.toLong(), MTU.toLong())
        datapath = engine
        engine.setLightMode(false) // Bridge transports traffic; the selected VPN owns routing.
        progress("4/5 · Привязываю к VPN", "Проверяю доступ через выбранный VPN…")
        engine.setNetwork(vpn.networkHandle)
        engine.verifyBinding()
        DiagnosticJournal.record("bridge-stage", "datapath bound to VPN network")

        progress("5/5 · Включаю точку доступа", "Ожидаю подтверждение Android и защищённого маршрута…")
        val activated = bridge.command("ACTIVATE")
        check(activated.startsWith("ACTIVE|")) { activated.removePrefix("ERROR|") }
        DiagnosticJournal.record("bridge-stage", "system tethering active on test upstream")
        check(findVpn() == vpn) { "VPN отключился во время запуска" }

        watchVpn(vpn)
        watchUpstream(bridge, prepared.interfaceName)
        BridgeRuntimeService.start(context)
        update(
            BridgeState(
                phase = BridgePhase.ACTIVE,
                title = "Hotspot защищён",
                detail = "Раздача через выбранный VPN. При его отключении hotspot остановится",
                vpnName = vpnName(vpn),
                interfaceName = prepared.interfaceName,
            ),
        )
    }

    private fun connectHelper(): ShellBridgeConnection {
        val socketName = residentSocketName()
        ShellBridgeConnection.connectResident(socketName)?.let { resident ->
            val probe = runCatching { resident.command("PROBE") }.getOrNull()
            if (probe?.startsWith("OK|") == true) return resident
            runCatching { resident.close() }
        }

        DiagnosticJournal.record("bridge-stage", "resident helper unavailable; discovering ADB connect endpoint")
        val endpoint = discoverEndpoint()
        DiagnosticJournal.record("bridge-stage", "ADB connect endpoint discovered port=${endpoint.second}")
        val hosts = linkedSetOf("127.0.0.1", "::1", endpoint.first)
        var last: Throwable? = null
        for (host in hosts) {
            var kadb: Kadb? = null
            try {
                val connection = Kadb.create(host, endpoint.second, connectTimeout = 5_000, socketTimeout = 0)
                kadb = connection
                val identity = connection.shell("id -u").allOutput.trim()
                check(identity == "2000") { "helper uid=$identity, ожидался shell uid=2000" }
                val transport = if (host == "127.0.0.1" || host == "::1") "loopback" else "local-network"
                DiagnosticJournal.record("bridge-stage", "ADB shell authenticated as uid 2000 transport=$transport")
                val pathResult = connection.shell("pm path ${context.packageName}")
                check(pathResult.exitCode == 0) { pathResult.errorOutput }
                val apkPath = pathResult.allOutput.lineSequence()
                    .firstOrNull { it.startsWith("package:") }
                    ?.removePrefix("package:")
                    ?: error("установленный APK не найден")
                DiagnosticJournal.record("bridge-stage", "installed APK path resolved; launching resident helper")
                return ShellBridgeConnection.launchResident(connection, apkPath, socketName, context.packageName)
            } catch (failure: Throwable) {
                runCatching { kadb?.close() }
                last = failure
            }
        }
        throw IllegalStateException("Wireless debugging недоступна: ${last?.message}", last)
    }

    private fun residentSocketName(): String =
        "argus_${context.packageName.replace('.', '_')}_resident_v$HELPER_VERSION"

    private fun discoverEndpoint(): Pair<String, Int> {
        preferences.getInt(KEY_CONNECT_PORT, 0).takeIf { it > 0 }?.let { savedPort ->
            runCatching {
                Kadb.create("127.0.0.1", savedPort, 2_000, 5_000).use { it.shell("true") }
            }.getOrNull()?.let { return "127.0.0.1" to savedPort }
        }

        val wifiConnected = connectivity.allNetworks.any { network ->
            connectivity.getNetworkCapabilities(network)?.hasTransport(NetworkCapabilities.TRANSPORT_WIFI) == true
        }
        check(wifiConnected) { "Мост остановлен. Для восстановления временно подключитесь к Wi‑Fi и повторите запуск. Затем можно раздавать мобильный интернет" }
        progress("Ищу системный мост", "Пара сохранена. Ожидаю адрес беспроводной отладки Android…")
        val found = discoverNsdEndpoint("_adb-tls-connect._tcp", 15,
            "Android не сообщил адрес отладки. Включите «Беспроводную отладку» в параметрах разработчика и повторите запуск")
        preferences.edit().putInt(KEY_CONNECT_PORT, found.second).apply()
        return found
    }

    private fun discoverNsdEndpoint(serviceType: String, timeoutSeconds: Long, failureMessage: String): Pair<String, Int> {
        val nsd = context.getSystemService(NsdManager::class.java)
        val multicast = runCatching {
            context.getSystemService(WifiManager::class.java)
                .createMulticastLock("argus-adb-discovery")
                .apply { setReferenceCounted(false) }
        }.getOrNull()
        val endpoint = AtomicReference<Pair<String, Int>?>()
        val completed = CountDownLatch(1)
        val listener = object : NsdManager.DiscoveryListener {
            override fun onDiscoveryStarted(serviceType: String) = Unit
            override fun onDiscoveryStopped(serviceType: String) = Unit
            override fun onStartDiscoveryFailed(serviceType: String, errorCode: Int) = completed.countDown()
            override fun onStopDiscoveryFailed(serviceType: String, errorCode: Int) = Unit

            override fun onServiceLost(serviceInfo: NsdServiceInfo) = Unit

            override fun onServiceFound(serviceInfo: NsdServiceInfo) {
                @Suppress("DEPRECATION")
                nsd.resolveService(serviceInfo, object : NsdManager.ResolveListener {
                    override fun onResolveFailed(info: NsdServiceInfo, errorCode: Int) = Unit

                    override fun onServiceResolved(info: NsdServiceInfo) {
                        @Suppress("DEPRECATION")
                        val host = if (Build.VERSION.SDK_INT >= 34) info.hostAddresses.firstOrNull()?.hostAddress else info.host?.hostAddress
                        if (!host.isNullOrBlank() && info.port > 0 && endpoint.compareAndSet(null, host to info.port)) {
                            completed.countDown()
                        }
                    }
                })
            }
        }
        try {
            multicast?.acquire()
            nsd.discoverServices(serviceType, NsdManager.PROTOCOL_DNS_SD, listener)
            check(completed.await(timeoutSeconds, TimeUnit.SECONDS)) { "$failureMessage через mDNS" }
            return endpoint.get() ?: error(failureMessage)
        } finally {
            runCatching { nsd.stopServiceDiscovery(listener) }
            runCatching { if (multicast?.isHeld == true) multicast.release() }
        }
    }

    private fun isUsableVpn(network: Network): Boolean {
        val caps = connectivity.getNetworkCapabilities(network) ?: return false
        return caps.hasTransport(NetworkCapabilities.TRANSPORT_VPN) &&
            caps.hasCapability(NetworkCapabilities.NET_CAPABILITY_INTERNET)
    }

    private fun findVpn(): Network? {
        connectivity.activeNetwork?.takeIf(::isUsableVpn)?.let { return it }
        // Never pick an arbitrary VPN from multiple profiles/networks.
        return connectivity.allNetworks.filter(::isUsableVpn).singleOrNull()
    }

    private fun vpnName(network: Network): String {
        val caps = connectivity.getNetworkCapabilities(network)
        return caps?.transportInfo?.toString()?.takeIf { it.isNotBlank() } ?: "Активный VPN"
    }

    private fun watchVpn(network: Network) {
        val request = NetworkRequest.Builder()
            .addTransportType(NetworkCapabilities.TRANSPORT_VPN)
            .removeCapability(NetworkCapabilities.NET_CAPABILITY_NOT_VPN)
            .addCapability(NetworkCapabilities.NET_CAPABILITY_INTERNET)
            .build()
        val callback = object : ConnectivityManager.NetworkCallback() {
            override fun onLost(lost: Network) {
                if (lost == network && state.isActive) stop("VPN отключился — hotspot закрыт")
            }
            override fun onCapabilitiesChanged(changed: Network, caps: NetworkCapabilities) {
                if (changed == network && state.isActive &&
                    (!caps.hasTransport(NetworkCapabilities.TRANSPORT_VPN) ||
                        !caps.hasCapability(NetworkCapabilities.NET_CAPABILITY_INTERNET))) {
                    stop("VPN недоступен — hotspot закрыт")
                }
            }
        }
        vpnCallback = callback
        connectivity.registerNetworkCallback(request, callback)
    }

    private fun watchUpstream(connection: ShellBridgeConnection, expected: String) {
        watchdog?.cancel()
        watchdog = scope.launch {
            while (true) {
                delay(5_000)
                if (vpnNetwork?.let(::isUsableVpn) != true || findVpn() != vpnNetwork) {
                    stop("Активный VPN изменился — включите раздачу заново для нового VPN")
                    return@launch
                }
                val observed = withContext(Dispatchers.IO) {
                    runCatching { connection.command("STATUS") }.getOrElse { "ERROR|${it.message}" }
                }
                if (observed != "ACTIVE|$expected") {
                    stop("Маршрут hotspot изменился — соединение закрыто")
                    return@launch
                }
            }
        }
    }

    private fun stopInternal() {
        watchdog?.cancel()
        watchdog = null
        vpnCallback?.let { runCatching { connectivity.unregisterNetworkCallback(it) } }
        vpnCallback = null
        vpnNetwork = null
        runCatching { helper?.close() }
        helper = null
        runCatching { datapath?.stop() }
        datapath = null
        runCatching { tun?.close() }
        tun = null
    }

    private fun fail(title: String, detail: String) {
        update(BridgeState(BridgePhase.ERROR, title, detail))
        BridgeRuntimeService.stop(context)
    }

    private fun update(next: BridgeState) {
        state = next
        DiagnosticJournal.record(
            "bridge",
            "phase=${next.phase} paired=${hasSavedPairing()} interface=${!next.interfaceName.isNullOrBlank()} " +
                "title=${DiagnosticJournal.redact(next.title)} detail=${DiagnosticJournal.redact(next.detail)}",
        )
        listeners.forEach { listener -> runCatching { listener(next) } }
        ArgusTileService.refresh(context)
        BridgeRuntimeService.refresh(context)
    }

    private fun progress(title: String, detail: String) {
        update(state.copy(phase = BridgePhase.STARTING, title = title, detail = detail))
    }

    private fun bridgeFailure(failure: Throwable): String {
        val messages = generateSequence(failure) { it.cause }.joinToString(" ") { it.message.orEmpty() }
        if (messages.contains("CERTIFICATE_UNKNOWN", true) || messages.contains("certificate unknown", true)) {
            preferences.edit().putBoolean(KEY_PAIRED, false).apply()
            return "Android отклонил сохранённый ключ. Откройте настройку раздачи и повторите сопряжение. Это ошибка доступа к мосту, а не мобильного интернета или VPN"
        }
        if (generateSequence(failure) { it.cause }.any { it is android.os.DeadObjectException }) {
            return "Android остановил системный мост. Повторите запуск; если мост недоступен, временно подключитесь к Wi‑Fi для восстановления. VPN на телефоне не изменён"
        }
        return readable(failure)
    }

    private fun readable(failure: Throwable): String {
        var root = failure
        while (root.cause != null && root.cause !== root) root = root.cause!!
        return root.message?.take(240) ?: root.javaClass.simpleName
    }

    companion object {
        private const val MTU = 1500
        private const val KEY_PAIRED = "adb_paired"
        private const val KEY_CONNECT_PORT = "adb_connect_port"
        private const val KEY_HELPER_VERSION = "helper_version"
        private const val HELPER_VERSION = 9
    }
}
