package com.argus.connect.bridge

import android.net.LocalServerSocket
import android.net.LocalSocket
import android.net.LocalSocketAddress
import java.io.BufferedReader
import java.io.BufferedWriter
import java.io.InputStreamReader
import java.io.OutputStreamWriter

/** Entry point launched by app_process through the app's private local-ADB session. */
object BridgeMain {
    @JvmStatic
    fun main(arguments: Array<String>) {
        try {
            run(arguments)
        } catch (failure: Throwable) {
            System.err.println("ARGUS_HELPER_FATAL|${describe(failure)}")
            failure.printStackTrace(System.err)
            System.err.flush()
            throw failure
        }
    }

    private fun run(arguments: Array<String>) {
        if (arguments.firstOrNull() == "--binder") {
            HiddenApiAccess.unseal()
            BridgeBinderHost(HiddenApiAccess.shellContext(), arguments[1]).publish()
            android.os.Looper.loop()
            return
        }
        if (arguments.firstOrNull() == "--self-test") {
            HiddenApiAccess.unseal()
            val result = PlatformBridge(HiddenApiAccess.shellContext()).probe()
            check(result.startsWith("OK|")) { result.removePrefix("ERROR|") }
            println("ARGUS_SELF_TEST_OK|$result")
            System.out.flush()
            return
        }
        val socketName = arguments.firstOrNull() ?: error("missing socket name")
        HiddenApiAccess.unseal()
        val platform = PlatformBridge(HiddenApiAccess.shellContext())
        val listener = LocalSocket()
        listener.bind(LocalSocketAddress(socketName, LocalSocketAddress.Namespace.ABSTRACT))
        val server = LocalServerSocket(listener.fileDescriptor)

        println("ARGUS_HELPER_READY")
        System.out.flush()

        var running = true
        try {
            while (running) server.accept().use { client ->
                val input = BufferedReader(InputStreamReader(client.inputStream))
                val output = BufferedWriter(OutputStreamWriter(client.outputStream))
                try {
                    while (true) {
                        when (input.readLine() ?: break) {
                            "PROBE" -> reply(output, platform.probe())
                            "PREPARE" -> {
                                runCatching { platform.prepare() }
                                    .onSuccess { prepared ->
                                        client.setFileDescriptorsForSend(
                                            arrayOf(prepared.descriptor.fileDescriptor),
                                        )
                                        reply(output, "TUN|${prepared.interfaceName}")
                                        client.setFileDescriptorsForSend(null)
                                    }
                                    .onFailure { failure -> reply(output, "ERROR|${describe(failure)}") }
                            }
                            "ACTIVATE" -> reply(output, runCatching { platform.activate() }
                                .getOrElse { "ERROR|${describe(it)}" })
                            "STATUS" -> reply(output, platform.status())
                            "TEARDOWN", "STOP" -> {
                                platform.stop()
                                reply(output, "OK|stopped")
                            }
                            "SHUTDOWN" -> {
                                platform.stop()
                                reply(output, "OK|shutdown")
                                running = false
                                break
                            }
                            else -> reply(output, "ERROR|unknown command")
                        }
                    }
                } finally {
                    // If the app process or datapath disappears, never leave a
                    // tethering route behind. The daemon itself remains ready.
                    platform.stop()
                }
            }
        } finally {
            runCatching { platform.stop() }
            runCatching { server.close() }
            runCatching { listener.close() }
        }
    }

    private fun reply(writer: BufferedWriter, value: String) {
        writer.write(value.replace('\n', ' ').replace('\r', ' '))
        writer.newLine()
        writer.flush()
    }

    private fun describe(failure: Throwable): String {
        var root = failure
        while (root.cause != null && root.cause !== root) root = root.cause!!
        return "${root.javaClass.simpleName}: ${root.message}".take(400)
    }
}
