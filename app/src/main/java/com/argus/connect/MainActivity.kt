package com.argus.connect

import android.Manifest
import android.app.Activity
import android.app.AlertDialog
import android.content.ClipData
import android.content.Intent
import android.content.pm.PackageManager
import android.graphics.Color
import android.graphics.Typeface
import android.graphics.drawable.GradientDrawable
import android.net.VpnService
import android.os.Bundle
import android.provider.Settings
import android.text.SpannableStringBuilder
import android.text.Spanned
import android.text.InputType
import android.text.style.ForegroundColorSpan
import android.view.Gravity
import android.view.View
import android.view.ViewGroup
import android.widget.EditText
import android.widget.CheckBox
import android.widget.ImageView
import android.widget.FrameLayout
import android.widget.LinearLayout
import android.widget.ScrollView
import android.widget.TextView
import androidx.core.content.FileProvider

class MainActivity : Activity() {
    private val app get() = application as ArgusApplication
    private val bridge get() = app.bridge
    private val light get() = app.light

    private lateinit var status: TextView
    private lateinit var detail: TextView
    private lateinit var modeLabel: TextView
    private lateinit var lightButton: UtilityActionView
    private lateinit var proxyButton: UtilityActionView
    private lateinit var hotspotButton: UtilityActionView
    private lateinit var diagnosticButton: UtilityActionView
    private lateinit var telegramRow: TextView
    private lateinit var whatsappRow: TextView
    private lateinit var signalRow: TextView
    private lateinit var threemaRow: TextView
    private lateinit var youtubeRow: TextView
    private lateinit var hotspotStatus: TextView
    private lateinit var updateStatus: TextView
    private lateinit var updates: AppUpdates
    private lateinit var proof: TextView
    private lateinit var hero: ArgusHeroView
    private lateinit var repairDeck: RepairDeckView
    private var bridgeObservation: AutoCloseable? = null
    private var lightObservation: AutoCloseable? = null

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        window.statusBarColor = getColor(R.color.argus_background)
        window.navigationBarColor = getColor(R.color.argus_background)
        setContentView(buildScreen())
        updates = AppUpdates(this) { message -> updateStatus.text = message }
        updateStatus.setOnClickListener { updates.onTap() }
        updates.check()

        lightButton.setOnClickListener { toggleLight() }
        hero.setOnClickListener {
            if (light.current().phase == LightPhase.ANALYZING) light.stop()
            toggleLight()
        }
        hotspotButton.setOnClickListener {
            when (bridge.current().phase) {
                BridgePhase.SETUP_REQUIRED, BridgePhase.ERROR -> {
                    if (bridge.hasSavedPairing()) bridge.start() else showPairingDialog()
                }
                else -> bridge.toggle()
            }
        }
        hotspotButton.setOnLongClickListener {
            AlertDialog.Builder(this)
                .setTitle("Сбросить bridge?")
                .setMessage("Сохранённая ADB-пара будет удалена только из Argus.")
                .setNegativeButton("Отмена", null)
                .setPositiveButton("Сбросить") { _, _ -> bridge.clearPairing() }
                .show()
            true
        }
        diagnosticButton.setOnClickListener { shareDiagnosticReport() }
        proxyButton.setOnClickListener { showProxyDialog() }
        requestRuntimePermissions()
        handleTileIntent()
    }

    override fun onDestroy() {
        if (::updates.isInitialized) updates.close()
        super.onDestroy()
    }

    override fun onNewIntent(intent: Intent) {
        super.onNewIntent(intent)
        setIntent(intent)
        handleTileIntent()
    }


    private fun handleTileIntent() {
        if (intent.action != ArgusTileService.ACTION_ENABLE_LIGHT) return
        intent.action = null
        val current = light.current()
        if (!current.isRunning && !current.isBusy) toggleLight()
    }

    override fun onStart() {
        super.onStart()
        bridgeObservation = bridge.observe { runOnUiThread { render() } }
        lightObservation = light.observe { runOnUiThread { render() } }
    }

    override fun onStop() {
        bridgeObservation?.close()
        lightObservation?.close()
        bridgeObservation = null
        lightObservation = null
        super.onStop()
    }

    @Deprecated("VpnService permission uses the platform activity result contract")
    override fun onActivityResult(requestCode: Int, resultCode: Int, data: Intent?) {
        super.onActivityResult(requestCode, resultCode, data)
        if (requestCode == VPN_PERMISSION && resultCode == RESULT_OK) light.start()
    }

    private fun toggleLight() {
        val current = light.current()
        if (current.isRunning || current.isBusy) {
            light.stop()
            return
        }
        if (bridge.current().isActive) {
            AlertDialog.Builder(this)
                .setTitle("Hotspot уже активен")
                .setMessage("Выключите защищённый hotspot перед запуском LIGHT.")
                .setPositiveButton("Понятно", null)
                .show()
            return
        }
        val permission = VpnService.prepare(this)
        if (permission == null) light.start() else startActivityForResult(permission, VPN_PERMISSION)
    }

    private fun buildScreen(): View {
        val content = object : LinearLayout(this) {
            override fun onMeasure(widthMeasureSpec: Int, heightMeasureSpec: Int) {
                super.onMeasure(View.MeasureSpec.makeMeasureSpec(minOf(View.MeasureSpec.getSize(widthMeasureSpec), dp(840)), View.MeasureSpec.EXACTLY), heightMeasureSpec)
            }
        }.apply {
            orientation = LinearLayout.VERTICAL
            gravity = Gravity.CENTER_HORIZONTAL
            setPadding(dp(22), dp(24), dp(22), dp(28))
        }
        val scroll = ScrollView(this).apply {
            setBackgroundColor(getColor(R.color.argus_background))
            isFillViewport = true
            addView(FrameLayout(this@MainActivity).apply {
                addView(content, FrameLayout.LayoutParams(ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.WRAP_CONTENT, Gravity.TOP or Gravity.CENTER_HORIZONTAL))
            }, ViewGroup.LayoutParams(ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.WRAP_CONTENT))
        }

        content.addView(TextView(this).apply {
            text = "ARGUS CONNECT"
            setTextColor(getColor(R.color.argus_muted))
            textSize = 13f
            letterSpacing = .22f
            typeface = Typeface.DEFAULT_BOLD
            gravity = Gravity.CENTER
        }, margins(ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.WRAP_CONTENT, bottom = dp(10)))

        hero = ArgusHeroView(this)
        content.addView(hero, margins(ViewGroup.LayoutParams.MATCH_PARENT, dp(184), bottom = dp(10)))

        modeLabel = TextView(this).apply {
            text = "ARGUS  •  АВТОДИАГНОСТИКА"
            setTextColor(getColor(R.color.argus_green))
            textSize = 12f
            letterSpacing = .1f
            typeface = Typeface.DEFAULT_BOLD
            gravity = Gravity.CENTER
        }
        content.addView(modeLabel, margins(ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.WRAP_CONTENT, bottom = dp(12)))

        status = TextView(this).apply {
            setTextColor(getColor(R.color.argus_text))
            textSize = 26f
            typeface = Typeface.DEFAULT_BOLD
            gravity = Gravity.CENTER
        }
        content.addView(status, margins(ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.WRAP_CONTENT, bottom = dp(10)))

        detail = TextView(this).apply {
            setTextColor(getColor(R.color.argus_muted))
            textSize = 13f
            gravity = Gravity.CENTER
            maxWidth = dp(560)
            setLineSpacing(0f, 1.12f)
        }
        content.addView(detail, margins(ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.WRAP_CONTENT, bottom = dp(24)))

        hotspotStatus = TextView(this).apply {
            textSize = 13f
            setPadding(dp(14), dp(12), dp(14), dp(12))
            background = rounded(getColor(R.color.argus_surface), dp(18).toFloat())
            setLineSpacing(0f, 1.12f)
            accessibilityLiveRegion = View.ACCESSIBILITY_LIVE_REGION_POLITE
            setOnClickListener {
                val current = bridge.current()
                AlertDialog.Builder(this@MainActivity).setTitle(current.title)
                    .setMessage(current.detail)
                    .setPositiveButton("Понятно", null).show()
            }
        }
        content.addView(hotspotStatus, margins(ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.WRAP_CONTENT, bottom = dp(18)))

        // Existing detailed reports remain available on each graphical card.
        telegramRow = checkRow()
        whatsappRow = checkRow()
        signalRow = checkRow()
        threemaRow = checkRow()
        youtubeRow = checkRow()
        repairDeck = RepairDeckView(this) { key ->
            val report: CharSequence = when (key) {
                "telegram" -> telegramRow.text
                "whatsapp" -> whatsappRow.text
                "youtube" -> youtubeRow.text
                "signal" -> signalRow.text
                "facebook" -> socialStatus("Facebook", light.current().facebook)
                "instagram" -> socialStatus("Instagram", light.current().instagram)
                "twitter" -> socialStatus("X / Twitter", light.current().twitter)
                else -> threemaRow.text
            }
            AlertDialog.Builder(this).setTitle("Результаты проверки")
                .setMessage(report).setPositiveButton("Понятно", null).show()
        }
        content.addView(repairDeck, margins(ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.WRAP_CONTENT, bottom = dp(20)))

        lightButton = UtilityActionView(this, "power")
        hotspotButton = UtilityActionView(this, "hotspot")
        proxyButton = UtilityActionView(this, "routes")
        diagnosticButton = UtilityActionView(this, "report").apply {
            text = "ОТПРАВИТЬ ТЕХНИЧЕСКИЙ ОТЧЁТ"
        }

        updateStatus = TextView(this).apply {
            text = "Версия ${BuildConfig.VERSION_NAME} · проверить обновления"
            textSize = 12f
            gravity = Gravity.CENTER
            setTextColor(getColor(R.color.argus_muted))
            setPadding(dp(8), dp(12), dp(8), dp(12))
        }
        content.addView(updateStatus, margins(ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.WRAP_CONTENT))
        proof = TextView(this).apply {
            setTextColor(getColor(R.color.argus_muted))
            textSize = 11f
            gravity = Gravity.CENTER
        }
        content.addView(proof, margins(ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.WRAP_CONTENT))
        val dock = object : LinearLayout(this) {
            override fun onMeasure(widthMeasureSpec: Int, heightMeasureSpec: Int) {
                super.onMeasure(View.MeasureSpec.makeMeasureSpec(minOf(View.MeasureSpec.getSize(widthMeasureSpec), dp(340)), View.MeasureSpec.EXACTLY), heightMeasureSpec)
            }
        }.apply {
            gravity = Gravity.CENTER
            setPadding(dp(8), dp(8), dp(8), dp(8))
            background = rounded(getColor(R.color.argus_background), dp(28).toFloat())
            elevation = dp(12).toFloat()
            listOf(lightButton, hotspotButton, proxyButton, diagnosticButton).forEach {
                addView(it, LinearLayout.LayoutParams(0, dp(56), 1f).apply { marginStart = dp(4); marginEnd = dp(4) })
            }
        }
        return FrameLayout(this).apply {
            setBackgroundColor(getColor(R.color.argus_background))
            addView(scroll, FrameLayout.LayoutParams(ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.MATCH_PARENT).apply { bottomMargin = dp(92) })
            addView(dock, FrameLayout.LayoutParams(ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.WRAP_CONTENT, Gravity.BOTTOM or Gravity.CENTER_HORIZONTAL).apply { bottomMargin = dp(12); leftMargin=dp(16); rightMargin=dp(16) })
            setOnApplyWindowInsetsListener { view, insets ->
                val bars = insets.getInsets(android.view.WindowInsets.Type.systemBars() or android.view.WindowInsets.Type.displayCutout())
                view.setPadding(bars.left, bars.top, bars.right, bars.bottom)
                insets
            }
        }
    }

    private fun render() {
        val lightState = light.current()
        val bridgeState = bridge.current()
        val showingLight = lightState.phase != LightPhase.OFF
        modeLabel.text = when (lightState.phase) {
            LightPhase.ANALYZING, LightPhase.BASELINE -> "ИСХОДНАЯ СЕТЬ  •  БЕЗ ОБХОДА"
            LightPhase.OFF -> "ARGUS  •  АВТОДИАГНОСТИКА"
            else -> if (light.proxyCount() > 0 || light.publicMTProxyDiscovery()) "LIGHT  •  МАРШРУТЫ ПО СЕРВИСАМ" else "LIGHT  •  IP НЕ МЕНЯЕТСЯ"
        }
        val repairing = lightState.phase == LightPhase.STARTING || (lightState.isRunning &&
            (!lightState.automaticChecksComplete || listOf(lightState.facebook, lightState.instagram, lightState.twitter).any { it.page == CheckResult.WAITING }))
        val primaryChecks = listOf(lightState.telegram.messages, lightState.youtube, lightState.whatsapp.messages,
            lightState.signal.messages, lightState.threema.messages, lightState.facebook.page, lightState.instagram.page, lightState.twitter.page)
        val passed = primaryChecks.count { it == CheckResult.WORKS }
        val routes = primaryChecks.count { it == CheckResult.ROUTE || it == CheckResult.FALLBACK }
        val failed = primaryChecks.count { it == CheckResult.FAILED }
        val tested = primaryChecks.count { it != CheckResult.WAITING }
        status.text = when {
            repairing -> "Чиню твои\nлюбимые сервисы"
            lightState.phase == LightPhase.LIMITED -> "LIGHT включён\nЕсть ограничения"
            lightState.isRunning -> "LIGHT включён"
            lightState.phase == LightPhase.ANALYZING -> "Знакомлюсь с твоей сетью"
            lightState.phase == LightPhase.BASELINE -> "Сеть проверена"
            showingLight -> lightState.title
            else -> "Всё начинается\nс хорошей связи"
        }
        detail.text = when {
            repairing -> "Проверено $tested из 8 сервисов · подбираю соединение"
            lightState.isRunning -> buildList {
                if (passed > 0) add("$passed проверено")
                if (routes > 0) add("$routes — маршрут найден")
                if (failed > 0) add("$failed — тест не пройден")
                if (tested < 8) add("${8-tested} ожидают проверки")
            }.joinToString(" · ")
            lightState.phase == LightPhase.BASELINE -> "Нажми на щит, чтобы включить LIGHT"
            showingLight -> lightState.detail
            else -> "Нажми на щит — проверю твои сервисы"
        }
        hero.render(lightState, repairing || lightState.phase == LightPhase.ANALYZING)
        hero.stateDescription = status.text
        modeLabel.setTextColor(when {
            repairing || lightState.phase == LightPhase.LIMITED -> Color.rgb(246,194,112)
            lightState.isRunning -> getColor(R.color.argus_green)
            else -> getColor(R.color.argus_muted)
        })
        hero.isEnabled = lightState.phase !in listOf(LightPhase.STARTING, LightPhase.STOPPING)
        hero.contentDescription = if (lightState.isRunning) "Остановить LIGHT" else "Включить LIGHT"
        val baselineProblem = lightState.phase == LightPhase.BASELINE && listOf(
            lightState.telegram.messages,
            lightState.whatsapp.messages,
            lightState.whatsappMedia,
            lightState.signal.messages,
            lightState.threema.messages,
            lightState.youtubeFeed,
            lightState.youtubeThumbnails,
            lightState.youtubeVideo,
        ).any { it == CheckResult.FAILED }
        val isProblem = lightState.phase == LightPhase.ERROR || lightState.phase == LightPhase.LIMITED || baselineProblem
        status.setTextColor(getColor(if (lightState.phase == LightPhase.ERROR) R.color.argus_error else R.color.argus_text))

        telegramRow.text = serviceStatus("Telegram", "telegram", lightState.telegram, lightState)
        whatsappRow.text = whatsappStatus(lightState)
        signalRow.text = serviceStatus("Signal", "signal", lightState.signal, lightState)
        threemaRow.text = serviceStatus("Threema", "threema", lightState.threema, lightState)
        youtubeRow.text = youtubeStatus(lightState)
        repairDeck.render(lightState)

        lightButton.text = when (lightState.phase) {
            LightPhase.ANALYZING -> "ОСТАНОВИТЬ АНАЛИЗ"
            LightPhase.STARTING -> "ЗАПУСК LIGHT…"
            LightPhase.STOPPING -> "ОСТАНОВКА…"
            LightPhase.VERIFYING, LightPhase.ACTIVE, LightPhase.LIMITED -> "ВЫКЛЮЧИТЬ LIGHT"
            else -> "ВКЛЮЧИТЬ LIGHT"
        }
        lightButton.isEnabled = !lightState.isBusy || lightState.phase == LightPhase.ANALYZING
        lightButton.alpha = if (lightButton.isEnabled) 1f else .55f
        lightButton.active = lightState.isRunning
        hotspotStatus.text = "Защищённая раздача · ${bridgeState.title}\n${bridgeState.detail}"
        hotspotStatus.setTextColor(getColor(when {
            bridgeState.phase == BridgePhase.ERROR -> R.color.argus_error
            bridgeState.isActive -> R.color.argus_green
            else -> R.color.argus_text
        }))
        hotspotButton.active = bridgeState.isActive
        proxyButton.active = light.proxyCount() > 0 || light.publicMTProxyDiscovery()

        hotspotButton.text = when (bridgeState.phase) {
            BridgePhase.SETUP_REQUIRED -> if (bridge.hasSavedPairing()) {
                "ВОССТАНОВИТЬ BRIDGE БЕЗ КОДА"
            } else {
                "НАСТРОИТЬ ЗАЩИЩЁННЫЙ HOTSPOT"
            }
            BridgePhase.ACTIVE -> "ВЫКЛЮЧИТЬ ЗАЩИЩЁННЫЙ HOTSPOT"
            BridgePhase.STARTING -> "HOTSPOT: ЗАПУСК…"
            BridgePhase.STOPPING -> "HOTSPOT: ОСТАНОВКА…"
            else -> "РАЗДАТЬ АКТИВНЫЙ VPN ПО WI-FI"
        }
        hotspotButton.alpha = if (bridgeState.isBusy) .55f else 1f
        proxyButton.text = if (light.publicMTProxyDiscovery()) {
            "ПРОКСИ-РЕЗЕРВ: ${light.proxyCount()} + АВТОПОИСК TG"
        } else if (light.proxyCount() == 0) {
            "ПРОКСИ-РЕЗЕРВ: ВЫКЛ"
        } else {
            "ПРОКСИ-РЕЗЕРВ: ${light.proxyCount()}"
        }

        proof.text = when {
            lightState.isRunning -> "Зелёный — тест пройден · жёлтый — нужны данные\nНажми на сервис для подробностей"
            lightState.phase == LightPhase.ANALYZING || lightState.phase == LightPhase.BASELINE -> "Исходная сеть · LIGHT ещё не включён"
            bridgeState.isActive -> "Защищённая раздача включена"
            else -> "LIGHT и раздача включаются отдельно"
        }
    }

    private fun socialStatus(label: String, check: SocialCheck): CharSequence = SpannableStringBuilder().apply {
        append("$label\n  сайт/API ")
        appendResult(this, check.page, "social", light.current())
        append("\n  медиа/CDN ")
        appendResult(this, check.media, "social", light.current(), waitingText = "— не проверено")
        append("\n\nПроверка сервера не подтверждает загрузку ленты или воспроизведение видео в приложении.")
    }

    private fun serviceStatus(
        label: String,
        key: String,
        check: ServiceCheck,
        state: LightState,
    ): CharSequence = SpannableStringBuilder().apply {
        append(label)
        append(if (key == "telegram") "\n  MTProto " else "\n  сообщения ")
        appendResult(this, check.messages, key, state)
        append("\n  звонки ")
        appendResult(this, check.calls, key, state, media = true)
        appendServiceDetail(this, key, check.messages, state)
    }

    private fun whatsappStatus(state: LightState): CharSequence = SpannableStringBuilder().apply {
        append("WhatsApp")
        append("\n  сообщения ")
        appendResult(this, state.whatsapp.messages, "whatsapp-messages", state)
        append("\n  медиа ")
        appendResult(
            this,
            state.whatsappMedia,
            "whatsapp-media-route",
            state,
            waitingText = "— откройте фото или видео",
        )
        append("\n  звонки ")
        appendResult(this, state.whatsapp.calls, "whatsapp", state, media = true)
        appendServiceDetail(this, "whatsapp", state.whatsapp.messages, state)
    }

    private fun youtubeStatus(state: LightState): CharSequence = SpannableStringBuilder().apply {
        append("YouTube")
        append("\n  лента ")
        appendResult(this, state.youtubeFeed, "youtube-feed", state)
        append("\n  обложки ")
        appendResult(this, state.youtubeThumbnails, "youtube-thumbnails", state)
        append("\n  видео ")
        appendResult(
            this,
            state.youtubeVideo,
            "youtube-video-route",
            state,
            waitingText = "— запустите ролик",
        )
        appendServiceDetail(this, "youtube", state.youtube, state)
    }

    private fun appendResult(
        text: SpannableStringBuilder,
        result: CheckResult,
        key: String,
        state: LightState,
        media: Boolean = false,
        waitingText: String? = null,
    ) {
        when (result) {
            CheckResult.WORKS -> text.colored(
                "✓ работает",
                R.color.argus_green,
            )
            CheckResult.ROUTE -> text.colored(
                if (key == "telegram") "○ тестовый канал отвечает; сообщения и медиа не подтверждены"
                else if (key.startsWith("youtube")) "○ маршрут отвечает; загрузка содержимого не подтверждена"
                else "○ тестовый маршрут отвечает; работа приложения не подтверждена",
                R.color.argus_muted,
            )
            CheckResult.FAILED -> text.colored("✕ не удалось", R.color.argus_error)
            CheckResult.FALLBACK -> text.colored("↪ TCP", R.color.argus_muted)
            CheckResult.WAITING -> {
                if (!media && key in state.recoveringServices) {
                    val attempt = state.probeAttempts[key] ?: 1
                    text.colored("○ $attempt/$PROBE_TOTAL", R.color.argus_muted)
                } else {
                    text.colored(
                        waitingText ?: if (media) "— нужен тестовый звонок" else "— не проверено",
                        R.color.argus_muted,
                    )
                }
            }
        }
    }

    private fun appendServiceDetail(
        text: SpannableStringBuilder,
        key: String,
        result: CheckResult,
        state: LightState,
    ) {
        if (key in state.recoveringServices) {
            val strategy = if (state.phase == LightPhase.ANALYZING) {
                "прямое соединение без обхода"
            } else {
                strategyLabel(state.currentStrategies[key].orEmpty())
            }
            text.append("\n  сейчас: ")
            text.colored(strategy, R.color.argus_muted)
        } else if (result == CheckResult.FAILED) {
            text.append("\n  итог: ")
            val failure = state.failureReasons[key] ?: "LIGHT не справился"
            val explanation = if (state.phase == LightPhase.BASELINE) {
                "$failure — LIGHT ещё не запускался"
            } else {
                "$failure — обход не подтверждён"
            }
            text.colored(explanation, R.color.argus_error)
        } else if (result == CheckResult.WORKS) {
            val route = routeLabel(state.currentRoutes[key].orEmpty())
            if (route.isNotEmpty()) {
                text.append("\n  маршрут: ")
                text.colored(route, R.color.argus_green)
            }
            val measuredKbps = state.throughputKbps[key]?.takeIf { it > 0 }
            if (measuredKbps != null) {
                val kbps = measuredKbps
                val speed = if (kbps >= 1_000) {
                    "%.1f Мбит/с".format(kbps / 1_000.0)
                } else {
                    "$kbps Кбит/с"
                }
                text.append(if (route.isNotEmpty()) "  •  " else "\n  скорость: ")
                when {
                    kbps < 1_000 -> text.colored("$speed — медленно", R.color.argus_error)
                    kbps < 2_500 -> text.colored("$speed — ограниченно", R.color.argus_muted)
                    else -> text.colored(speed, R.color.argus_green)
                }
            } else if (key == "youtube") {
                text.append("\n  скорость: ")
                text.colored("запустите видео для замера", R.color.argus_muted)
            }
        }
    }

    private fun routeLabel(route: String): String = when {
        route == "alternate-address" || route == "alternate-v4" -> "альтернативный IPv4"
        route == "alternate-v6" -> "прямой IPv6"
        route == "nat64-ipv6" -> "IPv6 через NAT64 оператора"
        route.startsWith("telegram-wss-dc") ->
            "официальный Telegram WSS, DC${route.substringAfterLast("dc")}"
        route.contains("-port-") -> {
            val base = route.substringBeforeLast("-port-")
            val baseLabel = routeLabel(base).ifEmpty { "основной адрес Telegram" }
            "$baseLabel, TCP/${route.substringAfterLast('-')}"
        }
        route.startsWith("telegram-dc") && route.contains("ipv6") ->
            "прямой IPv6 Telegram ${route.substringAfter("telegram-").substringBefore('-')}"
        route.startsWith("telegram-dc") && route.contains("v4-peer") ->
            "резервный IPv4 Telegram ${route.substringAfter("telegram-").substringBefore('-')}"
        route.startsWith("telegram-port-") -> "Telegram TCP/${route.substringAfterLast('-')}"
        route.startsWith("socks5-") -> "SOCKS резерв №${route.substringAfterLast('-')}"
        else -> ""
    }

    private fun strategyLabel(strategy: String): String = when (strategy) {
        "direct" -> "обычное соединение"
        "tls-record-split" -> "деление TLS-записи"
        "tls-multi-record" -> "TLS multi-record"
        "sni-split" -> "деление SNI"
        "sni-label-split" -> "деление имени сервера"
        "tcp-microburst" -> "TCP microburst"
        "mtproto-stagger" -> "MTProto stagger"
        "tcp-urgent-probe" -> "TCP urgent probe"
        "tcp-lowttl-disorder" -> "TCP disorder"
        "tls-record-sni-combo" -> "TLS record + SNI combo"
        "tls-record-sni-combo-fast" -> "быстрый TLS combo"
        "tls-record-sni-combo-balanced" -> "сбалансированный TLS combo"
        "tls-empty-record-prefix" -> "TLS empty-record"
        "tcp-small-mss" -> "мелкие TCP-сегменты"
        "tcp-split-1", "tcp-split-8", "tcp-split-mid" -> "TCP fragmentation"
        else -> "проверка IP/маршрута"
    }

    private fun SpannableStringBuilder.colored(value: String, color: Int) {
        val start = length
        append(value)
        setSpan(ForegroundColorSpan(getColor(color)), start, length, Spanned.SPAN_EXCLUSIVE_EXCLUSIVE)
    }

    private fun checkRow() = TextView(this).apply {
        setTextColor(getColor(R.color.argus_text))
        textSize = 13f
        typeface = Typeface.MONOSPACE
    }

    private fun actionButton() = TextView(this).apply {
        setTextColor(getColor(R.color.argus_background))
        textSize = 15f
        letterSpacing = .08f
        typeface = Typeface.DEFAULT_BOLD
        gravity = Gravity.CENTER
        isClickable = true
        isFocusable = true
    }

    private fun showProxyDialog() {
        val input = EditText(this).apply {
            setText(light.proxyPool())
            hint = "tg://proxy?server=…&port=443&secret=ee…"
            inputType = InputType.TYPE_CLASS_TEXT or
                InputType.TYPE_TEXT_VARIATION_URI or
                InputType.TYPE_TEXT_FLAG_MULTI_LINE
            minLines = 4
            maxLines = 8
            setHorizontallyScrolling(false)
            setPadding(dp(18), dp(12), dp(18), dp(12))
        }
        val discovery = CheckBox(this).apply {
            text = "Искать публичные MTProxy при сбое (только Telegram)"
            isChecked = light.publicMTProxyDiscovery()
            setTextColor(Color.DKGRAY)
        }
        val panel = LinearLayout(this).apply {
            orientation = LinearLayout.VERTICAL
            setPadding(dp(20), 0, dp(20), 0)
            addView(
                TextView(this@MainActivity).apply {
                    text = "По одной ссылке SOCKS5 или MTProxy (ee/FakeTLS) в строке. MTProxy работает внутри Argus только для Telegram; звонки отдельно. Публичному прокси видны IP и время соединений, его оператор не проверен."
                    setTextColor(Color.DKGRAY)
                    textSize = 13f
                },
                margins(ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.WRAP_CONTENT, bottom = dp(10)),
            )
            addView(input, margins(ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.WRAP_CONTENT))
            addView(discovery)
        }
        val dialog = AlertDialog.Builder(this)
            .setTitle("Прокси-резерв")
            .setView(panel)
            .setNegativeButton("Отмена", null)
            .setNeutralButton("Очистить", null)
            .setPositiveButton("Сохранить", null)
            .create()
        dialog.setOnShowListener {
            dialog.getButton(AlertDialog.BUTTON_NEUTRAL).setOnClickListener {
                light.saveProxyPool("", false)
                render()
                dialog.dismiss()
            }
            dialog.getButton(AlertDialog.BUTTON_POSITIVE).setOnClickListener {
                val count = light.saveProxyPool(input.text.toString(), discovery.isChecked)
                if (input.text.isNotBlank() && count == 0) {
                    input.error = "Нужны ссылки SOCKS5 или tg://proxy (ee/FakeTLS)"
                } else {
                    render()
                    dialog.dismiss()
                }
            }
        }
        dialog.show()
    }

    private fun showPairingDialog() {
        val panel = LinearLayout(this).apply {
            orientation = LinearLayout.VERTICAL
            setPadding(dp(24), dp(4), dp(24), 0)
        }
        panel.addView(TextView(this).apply {
            text = "Откройте системный код сопряжения. Порт Argus найдёт сам."
            setTextColor(Color.DKGRAY)
            textSize = 14f
        }, margins(ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.WRAP_CONTENT, bottom = dp(12)))
        val code = EditText(this).apply {
            hint = "Код — 6 цифр"
            inputType = InputType.TYPE_CLASS_NUMBER
            textSize = 20f
            gravity = Gravity.CENTER
            maxLines = 1
        }
        panel.addView(code)
        val dialog = AlertDialog.Builder(this)
            .setTitle("Подключить bridge")
            .setView(panel)
            .setNegativeButton("Отмена", null)
            .setNeutralButton("Открыть настройки", null)
            .setPositiveButton("Подключить", null)
            .create()
        dialog.setOnShowListener {
            dialog.getButton(AlertDialog.BUTTON_NEUTRAL).setOnClickListener {
                val settings = Intent(Settings.ACTION_APPLICATION_DEVELOPMENT_SETTINGS).apply {
                    addFlags(Intent.FLAG_ACTIVITY_NEW_TASK or Intent.FLAG_ACTIVITY_LAUNCH_ADJACENT)
                }
                runCatching { startActivity(settings) }.onFailure {
                    startActivity(Intent(Settings.ACTION_SETTINGS))
                }
            }
            dialog.getButton(AlertDialog.BUTTON_POSITIVE).setOnClickListener {
                val pairingCode = code.text.toString().trim()
                if (!pairingCode.matches(Regex("\\d{6}"))) {
                    code.error = "Нужны 6 цифр"
                } else {
                    bridge.pair(pairingCode)
                    dialog.dismiss()
                }
            }
        }
        dialog.show()
    }

    private fun shareDiagnosticReport() {
        runCatching {
            val file = DiagnosticReport.create(this, light.current(), bridge.current())
            val uri = FileProvider.getUriForFile(this, "$packageName.files", file)
            val send = Intent(Intent.ACTION_SEND).apply {
                type = "text/plain"
                putExtra(Intent.EXTRA_SUBJECT, "Argus Connect diagnostic ${BuildConfig.VERSION_NAME}")
                putExtra(Intent.EXTRA_TEXT, "Технический отчёт Argus без содержимого сообщений и персональных адресов.")
                putExtra(Intent.EXTRA_STREAM, uri)
                clipData = ClipData.newRawUri("Argus diagnostic", uri)
                addFlags(Intent.FLAG_GRANT_READ_URI_PERMISSION)
            }
            startActivity(Intent.createChooser(send, "Куда отправить отчёт"))
        }.onFailure { failure ->
            AlertDialog.Builder(this)
                .setTitle("Не удалось создать отчёт")
                .setMessage(failure.message ?: failure.javaClass.simpleName)
                .setPositiveButton("Понятно", null)
                .show()
        }
    }

    private fun requestRuntimePermissions() {
        val missing = (if (android.os.Build.VERSION.SDK_INT >= 33) listOf(
            Manifest.permission.POST_NOTIFICATIONS,
            Manifest.permission.NEARBY_WIFI_DEVICES,
        ) else emptyList()).filter { checkSelfPermission(it) != PackageManager.PERMISSION_GRANTED }
        if (missing.isNotEmpty()) requestPermissions(missing.toTypedArray(), 70)
    }

    private fun rounded(fill: Int, radius: Float) = GradientDrawable().apply {
        shape = GradientDrawable.RECTANGLE
        setColor(fill)
        cornerRadius = radius
    }

    private fun margins(width: Int, height: Int, bottom: Int = 0) =
        LinearLayout.LayoutParams(width, height).apply { bottomMargin = bottom }

    private fun dp(value: Int): Int = (value * resources.displayMetrics.density).toInt()

    companion object {
        private const val VPN_PERMISSION = 701
        private const val PROBE_TOTAL = 4
    }
}
