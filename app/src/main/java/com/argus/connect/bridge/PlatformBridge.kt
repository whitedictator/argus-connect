package com.argus.connect.bridge

import android.annotation.SuppressLint
import android.content.Context
import android.net.ConnectivityManager
import android.net.LinkAddress
import android.net.LinkProperties
import android.net.Network
import android.net.NetworkCapabilities
import android.net.NetworkRequest
import android.net.TetheringManager
import android.os.Binder
import android.os.Handler
import android.os.HandlerThread
import android.os.ParcelFileDescriptor
import android.os.Parcel
import android.os.IBinder
import java.net.InetAddress
import java.lang.reflect.Proxy
import java.util.concurrent.TimeUnit
import java.util.concurrent.CountDownLatch
import java.util.concurrent.Executor
import java.util.concurrent.FutureTask

internal data class HelperTun(
    val interfaceName: String,
    val descriptor: ParcelFileDescriptor,
)

@android.annotation.TargetApi(36)
internal class PlatformBridge(private val context: Context) {
    private val connectivity = context.getSystemService(ConnectivityManager::class.java)
    private val lifetime = Binder()
    private var tunObject: Any? = null
    private var tun: ParcelFileDescriptor? = null
    private var interfaceName: String? = null
    private var testNetwork: Network? = null
    private var keepAlive: ConnectivityManager.NetworkCallback? = null
    private var callbackThread: HandlerThread? = null
    private var active = false
    private var publicTetheringRequest: TetheringManager.TetheringRequest? = null
    @Volatile private var reportedUpstream: Network? = null
    private var tetheringEventCallback: Any? = null
    private val directExecutor = Executor { action -> action.run() }

    fun probe(): String = runCatching {
        check(android.os.Process.myUid() == 2000) { "requires authorized shell helper" }
        for (permission in listOf("android.permission.MANAGE_TEST_NETWORKS", "android.permission.NETWORK_SETTINGS", "android.permission.TETHER_PRIVILEGED")) {
            check(context.checkSelfPermission(permission) == android.content.pm.PackageManager.PERMISSION_GRANTED) {
                "required shell permission unavailable: $permission"
            }
        }
        val testClass = Class.forName("android.net.TestNetworkManager")
        val testManager = context.getSystemService("test_network")
            ?: error("TestNetworkManager service missing")
        check(testClass.methods.any { it.name == "createTunInterface" }) {
            "createTunInterface missing"
        }

        val tetherClass = Class.forName("android.net.TetheringManager")
        val tetherManager = context.getSystemService("tethering")
            ?: error("TetheringManager service missing")
        check(tetherClass.methods.any { it.name == "setPreferTestNetworks" }) {
            "setPreferTestNetworks missing"
        }
        "OK|test-network and tethering preference available"
    }.getOrElse { "ERROR|${rootMessage(it)}" }

    fun prepare(): HelperTun {
        stop()
        val managerClass = Class.forName("android.net.TestNetworkManager")
        val manager = context.getSystemService("test_network")
            ?: error("test_network service unavailable")
        val addresses = listOf(
            linkAddress("192.0.2.2", 24),
            linkAddress("2001:db8::2", 64),
        )

        val create = managerClass.methods.firstOrNull { method ->
            method.name == "createTunInterface" && method.parameterTypes.size == 1
        } ?: error("createTunInterface overload unavailable")
        val argument: Any = if (create.parameterTypes[0].isArray) {
            addresses.toTypedArray()
        } else {
            addresses
        }
        val created = create.invoke(manager, argument) ?: error("TUN creation returned null")
        tunObject = created
        val name = created.javaClass.getMethod("getInterfaceName").invoke(created) as String
        val descriptor = created.javaClass.getMethod("getFileDescriptor").invoke(created)
            as ParcelFileDescriptor
        interfaceName = name
        tun = descriptor

        val properties = LinkProperties().apply { setInterfaceName(name) }
        val addDns = LinkProperties::class.java.getMethod("addDnsServer", InetAddress::class.java)
        addDns.invoke(properties, InetAddress.getByName("1.1.1.1"))
        addDns.invoke(properties, InetAddress.getByName("2606:4700:4700::1111"))
        val setup = managerClass.methods.firstOrNull { method ->
            method.name == "setupTestNetwork" &&
                method.parameterTypes.size == 3 &&
                method.parameterTypes[0] == LinkProperties::class.java
        } ?: error("setupTestNetwork(LinkProperties, …) unavailable")
        keepAlive = requestKeepAlive()
        setup.invoke(manager, properties, true, lifetime)
        testNetwork = awaitNetwork(name)
        return HelperTun(name, descriptor)
    }

    fun activate(): String {
        val name = interfaceName ?: error("PREPARE must run first")
        registerTetheringCallback()
        stopWifiHotspot()
        Thread.sleep(1_000)
        setPreferTestNetworks(true)
        check(startWifiHotspot()) { "Android hotspot rejected startTetheredHotspot" }
        setPreferTestNetworks(true)

        val deadline = System.currentTimeMillis() + 15_000
        var observed = "none"
        while (System.currentTimeMillis() < deadline) {
            observed = observedUpstream()
            if (reportedUpstream == testNetwork || observed == name) {
                active = true
                return "ACTIVE|$name"
            }
            Thread.sleep(500)
        }
        stop()
        error("fail-closed: tethering upstream=$observed, expected $name")
    }

    fun status(): String = when {
        active -> {
            val expected = interfaceName.orEmpty()
            val observed = observedUpstream()
            if (reportedUpstream == testNetwork || observed == expected) {
                "ACTIVE|$expected"
            } else {
                stop()
                "ERROR|fail-closed: upstream drifted to $observed"
            }
        }
        interfaceName != null -> "PREPARED|${interfaceName.orEmpty()}"
        else -> "IDLE|stopped"
    }

    fun stop() {
        if (active || interfaceName != null) runCatching { stopWifiHotspot() }
        active = false
        reportedUpstream = null
        runCatching { setPreferTestNetworks(false) }
        unregisterTetheringCallback()

        val managerClass = runCatching { Class.forName("android.net.TestNetworkManager") }.getOrNull()
        val manager = runCatching { context.getSystemService("test_network") }.getOrNull()
        val network = testNetwork
        if (managerClass != null && manager != null && network != null) {
            runCatching {
                managerClass.getMethod("teardownTestNetwork", Network::class.java)
                    .invoke(manager, network)
            }
        }
        testNetwork = null

        keepAlive?.let { callback -> runCatching { connectivity.unregisterNetworkCallback(callback) } }
        keepAlive = null
        callbackThread?.quitSafely()
        callbackThread = null
        runCatching { tun?.close() }
        tun = null
        tunObject = null
        interfaceName = null
    }

    private fun awaitNetwork(name: String): Network {
        val deadline = System.currentTimeMillis() + 10_000
        while (System.currentTimeMillis() < deadline) {
            connectivity.allNetworks.firstOrNull { candidate ->
                connectivity.getLinkProperties(candidate)?.interfaceName == name
            }?.let { return it }
            Thread.sleep(200)
        }
        error("test network $name did not become available")
    }

    @SuppressLint("WrongConstant")
    private fun requestKeepAlive(): ConnectivityManager.NetworkCallback {
        val request = NetworkRequest.Builder()
            .clearCapabilities()
            .addTransportType(7)
            .build()
        val callback = object : ConnectivityManager.NetworkCallback() {}
        // Callbacks use a dedicated thread; legacy helpers may block the main
        // Looper, and Binder commands must never wait on their own callback thread.
        val thread = HandlerThread("argus-network-callback").apply { start() }
        callbackThread = thread
        connectivity.requestNetwork(request, callback, Handler(thread.looper))
        return callback
    }

    // Executed exclusively in the shell-UID app_process after HiddenApiAccess.
    // This private API is device-tested, not portable; failures stop the bridge.
    @SuppressLint("BlockedPrivateApi")
    private fun setPreferTestNetworks(prefer: Boolean) {
        check(android.os.Process.myUid() == 2000) { "requires shell helper" }
        val type = Class.forName("android.net.TetheringManager")
        val manager = context.getSystemService("tethering")
            ?: error("tethering service unavailable")
        val method = type.methods.firstOrNull { candidate ->
            candidate.name == "setPreferTestNetworks" && candidate.parameterTypes.size == 1
        } ?: error("setPreferTestNetworks unavailable")
        method.invoke(manager, prefer)
        // The platform void wrapper drops several non-security error codes.
        // Read the actual connector acknowledgement rather than assuming success.
        val field = type.getDeclaredField("mConnector").apply { isAccessible = true }
        val connector = field.get(manager) ?: error("tethering connector absent")
        val done = CountDownLatch(1)
        var result = -1
        val callbackBinder = object : Binder() {
            override fun onTransact(code: Int, data: Parcel, reply: Parcel?, flags: Int): Boolean {
                if (code != 1) return super.onTransact(code, data, reply, flags)
                data.enforceInterface("android.net.IIntResultListener")
                result = data.readInt()
                done.countDown()
                return true
            }
        }
        val callbackType = Class.forName("android.net.IIntResultListener")
        val callback = Proxy.newProxyInstance(callbackType.classLoader, arrayOf(callbackType)) { _, m, args ->
            when (m.name) {
                "asBinder" -> callbackBinder
                "onResult" -> { result = args!![0] as Int; done.countDown(); null }
                else -> null
            }
        }
        connector.javaClass.getMethod("setPreferTestNetworks", Boolean::class.javaPrimitiveType, callbackType)
            .invoke(connector, prefer, callback)
        check(done.await(3, TimeUnit.SECONDS)) { "setPreferTestNetworks acknowledgement timeout" }
        println("ARGUS_PREFER_TEST|prefer=$prefer result=$result")
        check(result == 0) { "setPreferTestNetworks rejected: Android code=$result" }
    }

    private fun startWifiHotspot(): Boolean {
        return startPublicTethering()
    }

    /**
     * Android's runtime callback contains an upstream-network event that is not
     * present in every public SDK stub. A dynamic proxy lets the shell helper
     * consume that event where the platform exposes it, while retaining the bounded
     * dumpsys fallback for platform variants that omit it.
     */
    private fun registerTetheringCallback() {
        if (tetheringEventCallback != null) return
        val type = Class.forName("android.net.TetheringManager\$TetheringEventCallback")
        val callback = Proxy.newProxyInstance(type.classLoader, arrayOf(type)) { proxy, method, arguments ->
            when (method.name) {
                "onUpstreamChanged" -> {
                    reportedUpstream = arguments?.firstOrNull() as? Network
                    null
                }
                "toString" -> "ArgusTetheringEventCallback"
                "hashCode" -> System.identityHashCode(proxy)
                "equals" -> proxy === arguments?.firstOrNull()
                else -> null
            }
        }
        val manager = context.getSystemService("tethering")
            ?: error("tethering service unavailable")
        val register = manager.javaClass.methods.firstOrNull { method ->
            method.name == "registerTetheringEventCallback" && method.parameterTypes.size == 2
        } ?: error("tethering callback unavailable")
        register.invoke(manager, directExecutor, callback)
        tetheringEventCallback = callback
    }

    private fun unregisterTetheringCallback() {
        val callback = tetheringEventCallback ?: return
        tetheringEventCallback = null
        runCatching {
            val manager = context.getSystemService("tethering") ?: return@runCatching
            manager.javaClass.methods.firstOrNull { method ->
                method.name == "unregisterTetheringEventCallback" && method.parameterTypes.size == 1
            }?.invoke(manager, callback)
        }
    }

    private fun startPublicTethering(): Boolean {
        val manager = context.getSystemService(TetheringManager::class.java)
        var lastFailure: String = "unknown"
        repeat(3) { attempt ->
            val builder = TetheringManager.TetheringRequest.Builder(TetheringManager.TETHERING_WIFI)
            // The helper runs as Android's privileged shell package. Suppress a
            // carrier provisioning window and use the existing hotspot config.
            builder.javaClass.methods.firstOrNull {
                it.name == "setExemptFromEntitlementCheck" && it.parameterTypes.size == 1 &&
                    it.parameterTypes[0] == Boolean::class.javaPrimitiveType
            }?.invoke(builder, true)
            builder.javaClass.methods.firstOrNull {
                it.name == "setShouldShowEntitlementUi" && it.parameterTypes.size == 1 &&
                    it.parameterTypes[0] == Boolean::class.javaPrimitiveType
            }?.invoke(builder, false)
            val request = builder.build()
            publicTetheringRequest = request
            val completed = CountDownLatch(1)
            var succeeded = false
            var failureCode: Int? = null
            manager.startTethering(
                request,
                directExecutor,
                object : TetheringManager.StartTetheringCallback {
                    override fun onTetheringStarted() {
                        succeeded = true
                        completed.countDown()
                    }

                    override fun onTetheringFailed(error: Int) {
                        failureCode = error
                        completed.countDown()
                    }
                },
            )
            if (!completed.await(15, TimeUnit.SECONDS)) {
                lastFailure = "timeout"
            } else if (succeeded) {
                return true
            } else {
                val code = failureCode ?: -1
                lastFailure = "${tetherErrorLabel(code)} (Android code=$code)"
                if (!retryableTetherError(code)) {
                    error("startTethering failed: $lastFailure")
                }
            }
            if (attempt < 2) {
                stopWifiHotspot()
                Thread.sleep(1_500L * (attempt + 1))
            }
        }
        error("startTethering failed after 3 attempts: $lastFailure")
    }

    private fun retryableTetherError(code: Int): Boolean = code == TetheringManager.TETHER_ERROR_SERVICE_UNAVAIL ||
        code == TetheringManager.TETHER_ERROR_UNAVAIL_IFACE ||
        code == TetheringManager.TETHER_ERROR_INTERNAL_ERROR ||
        code == TetheringManager.TETHER_ERROR_UNKNOWN_IFACE ||
        code == TetheringManager.TETHER_ERROR_DUPLICATE_REQUEST

    private fun tetherErrorLabel(code: Int): String = when (code) {
        TetheringManager.TETHER_ERROR_SERVICE_UNAVAIL -> "tethering service unavailable"
        TetheringManager.TETHER_ERROR_UNSUPPORTED -> "Wi-Fi tethering unsupported"
        TetheringManager.TETHER_ERROR_UNAVAIL_IFACE -> "hotspot interface unavailable"
        TetheringManager.TETHER_ERROR_INTERNAL_ERROR -> "Android tethering internal error"
        TetheringManager.TETHER_ERROR_PROVISIONING_FAILED -> "carrier provisioning rejected hotspot"
        TetheringManager.TETHER_ERROR_NO_CHANGE_TETHERING_PERMISSION -> "shell tether permission rejected"
        TetheringManager.TETHER_ERROR_NO_ACCESS_TETHERING_PERMISSION -> "shell network-state permission rejected"
        TetheringManager.TETHER_ERROR_DUPLICATE_REQUEST -> "previous hotspot request still active"
        TetheringManager.TETHER_ERROR_UNKNOWN_IFACE -> "Android hotspot interface not ready"
        else -> "unknown tethering error"
    }

    private fun stopWifiHotspot() {
        if (android.os.Build.VERSION.SDK_INT >= 36) publicTetheringRequest?.let { request ->
            runCatching {
                context.getSystemService(TetheringManager::class.java).stopTethering(
                    request,
                    directExecutor,
                    object : TetheringManager.StopTetheringCallback {},
                )
            }
        }
        publicTetheringRequest = null
        runCatching {
            val type = Class.forName("android.net.TetheringManager")
            val manager = context.getSystemService("tethering") ?: return@runCatching
            type.getMethod("stopTethering", Int::class.javaPrimitiveType).invoke(manager, 0)
        }
        runCatching {
            val manager = context.getSystemService(Context.WIFI_SERVICE) ?: return@runCatching
            manager.javaClass.methods.firstOrNull {
                it.name == "stopSoftAp" && it.parameterTypes.isEmpty()
            }?.invoke(manager)
        }
    }

    private fun tetheringUpstreams(): List<String> {
        val process = ProcessBuilder("dumpsys", "-t", "2", "tethering").redirectErrorStream(true).start()
        val read = FutureTask {
            process.inputStream.bufferedReader().use { reader ->
                val buffer = CharArray(65_536)
                var count = 0
                while (count < buffer.size) {
                    val n = reader.read(buffer, count, buffer.size - count)
                    if (n < 0) break
                    count += n
                }
                String(buffer, 0, count)
            }
        }
        Thread(read, "argus-tether-status").apply { isDaemon = true }.start()
        val output = try {
            read.get(3, TimeUnit.SECONDS)
        } catch (_: Exception) {
            ""
        } finally {
            if (process.isAlive) process.destroyForcibly()
            read.cancel(true)
        }

        val relevant = output.lineSequence().filter { line ->
            val value = line.trim().lowercase()
            value.startsWith("current upstream") ||
                value.startsWith("selected upstream") ||
                value.startsWith("upstream network") ||
                value.startsWith("mcurrentupstream")
        }
        val pattern = Regex("\\b(testtun\\d+|wlan\\d+|rmnet[a-z0-9_]*|eth\\d+|ap\\d+|swlan\\d+)\\b")
        return relevant.flatMap { pattern.findAll(it).map { match -> match.value } }
            .distinct()
            .toList()
    }

    private fun observedUpstream(): String {
        reportedUpstream?.let { network ->
            return connectivity.getLinkProperties(network)?.interfaceName ?: "network-${network.networkHandle}"
        }
        return tetheringUpstreams().singleOrNull() ?: "none"
    }

    private fun linkAddress(value: String, prefixLength: Int): LinkAddress {
        val constructor = LinkAddress::class.java.getDeclaredConstructor(
            InetAddress::class.java,
            Int::class.javaPrimitiveType,
        )
        constructor.isAccessible = true
        return constructor.newInstance(InetAddress.getByName(value), prefixLength)
    }

    private fun rootMessage(failure: Throwable): String {
        var root = failure
        while (root.cause != null && root.cause !== root) root = root.cause!!
        return "${root.javaClass.simpleName}: ${root.message}".take(400)
    }
}
