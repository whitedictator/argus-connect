package com.argus.connect

import android.net.LocalSocket
import android.net.LocalSocketAddress
import android.os.ParcelFileDescriptor
import android.os.IBinder
import android.os.Parcel
import com.flyfishxu.kadb.Kadb
import com.flyfishxu.kadb.shell.AdbShellPacket
import com.flyfishxu.kadb.shell.AdbShellStream
import java.io.BufferedReader
import java.io.BufferedWriter
import java.io.Closeable
import java.io.InputStreamReader
import java.io.OutputStreamWriter

internal data class PreparedTun(
    val interfaceName: String,
    val descriptor: ParcelFileDescriptor,
)

/** A client connection to the guarded shell-UID bridge daemon. */
internal class ShellBridgeConnection private constructor(
    private val socket: LocalSocket? = null,
    private val binder: IBinder? = null,
) : Closeable {
    private val input = socket?.let { BufferedReader(InputStreamReader(it.inputStream)) }
    private val output = socket?.let { BufferedWriter(OutputStreamWriter(it.outputStream)) }

    private fun transact(command: String): Pair<String, ParcelFileDescriptor?> {
        val request = Parcel.obtain()
        val response = Parcel.obtain()
        try {
            request.writeInterfaceToken("com.argus.connect.bridge.v9")
            request.writeString(command)
            check(binder!!.transact(1, request, response, 0)) { "bridge Binder rejected command" }
            response.readException()
            val result = response.readString() ?: error("empty bridge reply")
            return result to response.readTypedObject(ParcelFileDescriptor.CREATOR)
        } finally { request.recycle(); response.recycle() }
    }

    @Synchronized
    fun command(value: String): String {
        if (binder != null) return transact(value).let { (reply, fd) -> fd?.close(); reply }
        output!!.write(value)
        output.newLine()
        output.flush()
        return input!!.readLine() ?: error("bridge helper disconnected")
    }

    @Synchronized
    fun prepare(): PreparedTun {
        if (binder != null) {
            val (result, fd) = transact("PREPARE")
            check(result.startsWith("TUN|")) { result.removePrefix("ERROR|") }
            return PreparedTun(result.substringAfter('|'), fd ?: error("no Binder TUN descriptor"))
        }
        output!!.write("PREPARE")
        output.newLine()
        output.flush()

        val response = input!!.readLine() ?: error("bridge helper disconnected during PREPARE")
        check(response.startsWith("TUN|")) { response.removePrefix("ERROR|") }
        val received = socket!!.ancillaryFileDescriptors
        check(!received.isNullOrEmpty()) { "helper returned no TUN descriptor" }
        return PreparedTun(
            interfaceName = response.substringAfter('|'),
            descriptor = ParcelFileDescriptor.dup(received.first()),
        )
    }

    override fun close() {
        runCatching { command("TEARDOWN") }
        runCatching { socket?.close() }
    }

    companion object {
        private data class ResidentGuardian(
            val adb: Kadb,
            val shell: AdbShellStream,
            val transcript: StringBuilder,
        ) : Closeable {
            override fun close() {
                runCatching { shell.close() }
                runCatching { adb.close() }
            }
        }

        private val guardians = mutableMapOf<String, ResidentGuardian>()

        fun connectResident(socketName: String): ShellBridgeConnection? {
            if (socketName.endsWith("_v9")) {
                return BridgeBinderRegistry.helper?.takeIf { it.isBinderAlive }?.let { ShellBridgeConnection(binder = it) }
            }
            val socket = LocalSocket()
            return try {
                socket.connect(LocalSocketAddress(socketName, LocalSocketAddress.Namespace.ABSTRACT))
                socket.soTimeout = 5_000
                ShellBridgeConnection(socket)
            } catch (_: Throwable) {
                runCatching { socket.close() }
                null
            }
        }

        fun launchResident(kadb: Kadb, apkPath: String, socketName: String, packageName: String): ShellBridgeConnection {
            require(packageName.matches(Regex("[a-zA-Z0-9_.]+")))
            require(apkPath.startsWith("/data/app/") && !apkPath.contains(' ')) {
                "unexpected APK path: $apkPath"
            }
            require(socketName.matches(Regex("[a-zA-Z0-9_]{8,100}"))) { "invalid socket name" }

            synchronized(guardians) { guardians.remove(socketName)?.close() }
            var stream: AdbShellStream? = null
            try {
                val appProcess = "/system/bin/app_process -Djava.class.path=$apkPath /system/bin " +
                    "--nice-name=argus-bridge com.argus.connect.bridge.BridgeMain"
                // Prove that Android can load the APK, Kotlin runtime, hidden-API
                // bridge and shell context before starting a process that is meant
                // to stay alive. Unlike the resident stream, this command returns a
                // complete stdout/stderr/exit tuple that survives in diagnostics.
                val selfTest = kadb.shell("CLASSPATH=$apkPath $appProcess --self-test")
                val selfTestLog = buildString {
                    append("exit=${selfTest.exitCode}")
                    selfTest.output.trim().takeIf(String::isNotEmpty)?.let { append(" stdout=").append(it) }
                    selfTest.errorOutput.trim().takeIf(String::isNotEmpty)?.let { append(" stderr=").append(it) }
                }.replace('\n', ' ').replace('\r', ' ').take(2_400)
                DiagnosticJournal.record("bridge-helper-self-test", DiagnosticJournal.redact(selfTestLog))
                check(selfTest.exitCode == 0 && selfTest.output.contains("ARGUS_SELF_TEST_OK")) {
                    "app_process self-test failed: $selfTestLog"
                }

                // Detach from adbd's shell session. The Binder host still checks
                // the owner's UID and exits via owner.linkToDeath. Its lifetime
                // must not depend on Wi-Fi or a live ADB stream.
                runCatching {
                    kadb.shell("export CLASSPATH=$apkPath; /system/bin/setsid $appProcess --binder $packageName </dev/null >/dev/null 2>&1 & sleep 1")
                    repeat(30) {
                        val resident = connectResident(socketName)
                        if (resident != null) {
                            if (runCatching { resident.command("PROBE").startsWith("OK|") }.getOrDefault(false)) {
                                kadb.close()
                                DiagnosticJournal.record("bridge-helper", "independent Binder helper ready; no ADB guardian required")
                                return resident
                            }
                        }
                        Thread.sleep(100)
                    }
                }
                DiagnosticJournal.record("bridge-helper", "independent launch unavailable; trying guarded compatibility transport")

                // Compatibility fallback for firmware without setsid support.
                // This guarded transport may require Wi-Fi to remain available;
                // it is not evidence of independent mobile-data operation.
                val command = "export CLASSPATH=$apkPath; exec $appProcess --binder $packageName"
                val opened = kadb.openShell(command)
                stream = opened
                val transcript = StringBuilder()
                Thread({
                    while (true) {
                        val packet = runCatching { opened.read() }.getOrNull() ?: break
                        val text = when (packet) {
                            is AdbShellPacket.StdOut -> "stdout: ${packet.payload.toString(Charsets.UTF_8)}"
                            is AdbShellPacket.StdError -> "stderr: ${packet.payload.toString(Charsets.UTF_8)}"
                            is AdbShellPacket.Exit -> {
                                val code = packet.payload.firstOrNull()?.toInt()?.and(0xff) ?: -1
                                "exit=$code\n"
                            }
                            else -> "packet-id=${packet.id} bytes=${packet.payload.size}\n"
                        }
                        synchronized(transcript) {
                            transcript.append(text)
                            if (transcript.length > 2_400) {
                                transcript.delete(0, transcript.length - 2_400)
                            }
                        }
                    }
                }, "argus-bridge-guardian").apply {
                    isDaemon = true
                    start()
                }

                repeat(100) {
                    connectResident(socketName)?.let { resident ->
                        val guardian = ResidentGuardian(kadb, opened, transcript)
                        synchronized(guardians) { guardians[socketName] = guardian }
                        stream = null
                        return resident
                    }
                    Thread.sleep(100)
                }
                val log = synchronized(transcript) { transcript.toString() }
                    .replace('\n', ' ')
                    .replace('\r', ' ')
                    .takeLast(2_400)
                DiagnosticJournal.record(
                    "bridge-helper",
                    DiagnosticJournal.redact(log.ifBlank { "no process output" }),
                )
                error("resident helper did not start: ${log.ifBlank { "no process output" }}")
            } finally {
                stream?.let { runCatching { it.close() } }
                if (stream != null) runCatching { kadb.close() }
            }
        }
    }
}
