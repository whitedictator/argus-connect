package com.argus.connect

import android.animation.ValueAnimator
import android.content.Context
import android.graphics.Canvas
import android.graphics.Color
import android.graphics.Paint
import android.graphics.Path
import android.graphics.RectF
import android.graphics.Typeface
import android.graphics.drawable.GradientDrawable
import android.transition.ChangeBounds
import android.transition.Fade
import android.transition.TransitionManager
import android.transition.TransitionSet
import android.view.Gravity
import android.view.View
import android.view.ViewGroup
import android.view.animation.PathInterpolator
import android.graphics.SweepGradient
import android.graphics.RadialGradient
import android.graphics.Shader
import kotlin.math.cos
import kotlin.math.sin
import android.widget.GridLayout
import android.widget.FrameLayout
import android.widget.LinearLayout
import android.widget.TextView

/** Presentation only: observes existing checks; never starts probes or changes routes. */
internal class RepairDeckView(context: Context, private val onDetails: (String) -> Unit) : LinearLayout(context) {
    private val order = listOf("telegram", "youtube", "whatsapp", "signal", "threema", "facebook", "instagram", "twitter")
    private val names = mapOf("telegram" to "Telegram", "youtube" to "YouTube", "whatsapp" to "WhatsApp", "signal" to "Signal", "threema" to "Threema", "facebook" to "Facebook", "instagram" to "Instagram", "twitter" to "X / Twitter")
    private val activeSlot = LinearLayout(context).apply { orientation = VERTICAL }
    private val doneSlot = object : GridLayout(context) {
        private var previousColumns = 0
        private var previousChildren: List<View> = emptyList()
        override fun onMeasure(widthMeasureSpec: Int, heightMeasureSpec: Int) {
            val available = View.MeasureSpec.getSize(widthMeasureSpec)
            val columns = if (available >= px((560 * resources.configuration.fontScale.coerceAtLeast(1f)).toInt())) 2 else 1
            val children = (0 until childCount).map { getChildAt(it) }
            if (columns != previousColumns || children != previousChildren) {
            previousColumns = columns
            previousChildren = children
            if (columnCount != columns) columnCount = columns
            for (i in 0 until childCount) {
                getChildAt(i).layoutParams = GridLayout.LayoutParams().apply {
                    width = 0; height = ViewGroup.LayoutParams.WRAP_CONTENT
                    columnSpec = GridLayout.spec(i % columns, 1f)
                    rowSpec = GridLayout.spec(i / columns)
                    setGravity(Gravity.FILL_HORIZONTAL)
                    bottomMargin = px(8)
                    if (columns > 1) { leftMargin = px(4); rightMargin = px(4) }
                }
            }
            }
            super.onMeasure(widthMeasureSpec, heightMeasureSpec)
        }
    }.apply { columnCount = 1 }
    private val rail = LinearLayout(context).apply { gravity = Gravity.CENTER; setPadding(0, px(12), 0, px(16)) }
    private val caption = label("РЕЗУЛЬТАТЫ ПРОВЕРОК", 10f, muted).apply { letterSpacing = .15f; setPadding(px(4), px(12), 0, px(12)) }
    private val cards = order.associateWith { ServiceCard(it) }
    private val completed = linkedSetOf<String>()
    private var latest = LightState()
    private var bound = false
    private var pendingMove = false
    private var previousPhase = LightPhase.OFF
    private val move = Runnable {
        pendingMove = false
        if (!isAttachedToWindow || windowVisibility != VISIBLE) return@Runnable
        order.firstOrNull { it !in completed && terminal(it, latest) }?.let { completed += it }
        placeCards(animate = true)
        scheduleMove()
    }

    init {
        orientation = VERTICAL
        addView(activeSlot)
        addView(rail, LayoutParams(LayoutParams.MATCH_PARENT, LayoutParams.WRAP_CONTENT))
        addView(label("✓ проверено    ↗ сервер отвечает    … нет данных", 10f, muted).apply { gravity = Gravity.CENTER; setPadding(0, px(4), 0, px(8)) })
        addView(caption)
        addView(doneSlot)
    }

    fun render(state: LightState) {
        latest = state
        val reset = state.phase == LightPhase.STARTING && previousPhase != LightPhase.STARTING
        previousPhase = state.phase
        if (reset) { removeCallbacks(move); pendingMove = false; completed.clear() }
        if (!bound) {
            completed.addAll(order.filter { terminal(it, state) })
            bound = true
        }
        completed.removeAll { !terminal(it, state) }
        cards.forEach { (key, card) -> card.bind(state, key !in completed) }
        placeCards(animate = false)
        scheduleMove()
    }

    private fun scheduleMove() {
        if (!pendingMove && isAttachedToWindow && windowVisibility == VISIBLE && order.any { it !in completed && terminal(it, latest) }) {
            pendingMove = true
            postDelayed(move, if (ValueAnimator.areAnimatorsEnabled()) 520 else 0)
        }
    }

    private fun result(key: String, state: LightState): CheckResult = when (key) {
        "telegram" -> state.telegram.messages
        "youtube" -> state.youtube
        "whatsapp" -> state.whatsapp.messages
        "signal" -> state.signal.messages
        "facebook" -> state.facebook.page
        "instagram" -> state.instagram.page
        "twitter" -> state.twitter.page
        else -> state.threema.messages
    }

    private fun recovering(key: String, state: LightState) = state.recoveringServices.any { it == key || it.startsWith("$key-") }
    private fun terminal(key: String, state: LightState): Boolean {
        if (state.phase in listOf(LightPhase.OFF, LightPhase.STARTING, LightPhase.STOPPING)) return false
        if (key in listOf("facebook", "instagram", "twitter") && state.isRunning) return result(key, state) != CheckResult.WAITING
        return state.automaticChecksComplete || (!recovering(key, state) && result(key, state) != CheckResult.WAITING)
    }

    private fun placeCards(animate: Boolean) {
        if (animate && ValueAnimator.areAnimatorsEnabled() && isLaidOut) {
            TransitionManager.beginDelayedTransition(this, TransitionSet().apply {
                ordering = TransitionSet.ORDERING_TOGETHER
                addTransition(ChangeBounds().apply { setReparent(true) })
                addTransition(Fade())
                duration = 420
                interpolator = PathInterpolator(.22f, 1f, .36f, 1f)
            })
        }
        val activeKey = order.firstOrNull { it !in completed }
        activeSlot.visibility = if (activeKey == null || latest.phase == LightPhase.OFF) GONE else VISIBLE
        val desired = completed.toList()
        val active = activeKey?.let(cards::get)
        if (active != null && active.parent !== activeSlot) {
            (active.parent as? ViewGroup)?.removeView(active)
            activeSlot.removeAllViews()
            activeSlot.addView(active, LayoutParams(LayoutParams.MATCH_PARENT, LayoutParams.WRAP_CONTENT))
        }
        desired.forEachIndexed { index, key ->
            val card = cards.getValue(key)
            card.bind(latest, false)
            if (card.parent !== doneSlot) {
                (card.parent as? ViewGroup)?.removeView(card)
                doneSlot.addView(card, index, GridLayout.LayoutParams().apply { width = 0; height = ViewGroup.LayoutParams.WRAP_CONTENT; columnSpec = GridLayout.spec(0, 1f); bottomMargin = px(8) })
            }
        }
        for (i in doneSlot.childCount - 1 downTo 0) {
            if (doneSlot.getChildAt(i) !in desired.map(cards::get)) doneSlot.removeViewAt(i)
        }
        if (active != null) active.bind(latest, true)
        caption.visibility = if (completed.isEmpty()) GONE else VISIBLE
        // Small icons show the remaining queue, without invented progress.
        rail.removeAllViews()
        order.filter { it !in completed && it != activeKey }.forEach { key ->
            rail.addView(ServiceGlyph(context, key).apply { alpha = .38f; contentDescription = names[key] }, LayoutParams(px(28), px(28)).apply { marginStart = px(4); marginEnd = px(4) })
        }
        rail.visibility = if (rail.childCount > 0 && latest.phase != LightPhase.OFF) VISIBLE else GONE
        visibility = if (latest.phase == LightPhase.OFF) GONE else VISIBLE
    }

    override fun onAttachedToWindow() { super.onAttachedToWindow(); scheduleMove() }
    override fun onDetachedFromWindow() { removeCallbacks(move); pendingMove = false; TransitionManager.endTransitions(this); super.onDetachedFromWindow() }
    override fun onWindowVisibilityChanged(visibility: Int) {
        super.onWindowVisibilityChanged(visibility)
        if (visibility != VISIBLE) { removeCallbacks(move); pendingMove = false } else if (bound) scheduleMove()
    }

    private inner class ServiceCard(private val key: String) : LinearLayout(context) {
        private val glyph = ServiceGlyph(context, key)
        private val title = label(names.getValue(key), 16f, white).apply { typeface = Typeface.DEFAULT_BOLD }
        private val subtitle = label("Ожидает проверки", 12f, muted).apply { setLineSpacing(px(2).toFloat(), 1f) }
        private val mark = label("·", 22f, muted).apply { gravity = Gravity.CENTER; importantForAccessibility = IMPORTANT_FOR_ACCESSIBILITY_NO }
        init {
            orientation = HORIZONTAL
            gravity = Gravity.CENTER_VERTICAL
            setPadding(px(16), px(16), px(14), px(16))
            minimumHeight = px(80)
            val text = LinearLayout(context).apply { orientation = VERTICAL; addView(title); addView(subtitle, LayoutParams(LayoutParams.MATCH_PARENT, LayoutParams.WRAP_CONTENT).apply { topMargin = px(5) }) }
            addView(glyph, LayoutParams(px(42), px(42)).apply { marginEnd = px(14) })
            addView(text, LayoutParams(0, LayoutParams.WRAP_CONTENT, 1f))
            addView(mark, LayoutParams(px(28), px(28)).apply { marginStart = px(8) })
            isClickable = true
            isFocusable = true
            setOnClickListener { onDetails(key) }
        }
        fun bind(state: LightState, active: Boolean) {
            val r = result(key, state)
            val checking = !terminal(key, state)
            val color = when { checking -> amber; r == CheckResult.WORKS -> green; r == CheckResult.FAILED -> error; r == CheckResult.ROUTE || r == CheckResult.FALLBACK -> amber; else -> muted }
            mark.text = when { checking -> "···"; r == CheckResult.WORKS -> "✓"; r == CheckResult.FAILED -> "!"; r == CheckResult.ROUTE -> "↗"; else -> "—" }
            mark.setTextColor(color)
            glyph.confirmed = !checking && r == CheckResult.WORKS
            subtitle.text = when {
                checking && recovering(key, state) -> if (state.phase == LightPhase.ANALYZING) "Проверяю исходную сеть…" else "Подбираю рабочее соединение…"
                checking -> "В очереди на проверку"
                key == "youtube" -> "Лента ${symbol(state.youtubeFeed)} · обложки ${symbol(state.youtubeThumbnails)} · видео ${symbol(state.youtubeVideo)}"
                key in listOf("facebook", "instagram", "twitter") -> {
                    val social = when(key) { "facebook" -> state.facebook; "instagram" -> state.instagram; else -> state.twitter }
                    "Сайт/API ${symbol(social.page)} · медиа/CDN ${symbol(social.media)}"
                }
                r == CheckResult.WORKS -> if (state.phase == LightPhase.BASELINE) "Проверка сети пройдена" else "Данные передаются · подробнее ›"
                r == CheckResult.ROUTE -> "Маршрут отвечает · медиа и звонки не проверены"
                r == CheckResult.FAILED -> "Проверка не пройдена · подробнее ›"
                r == CheckResult.FALLBACK -> "Используется резервный маршрут"
                else -> "Не проверено · подробнее ›"
            }
            subtitle.setTextColor(if (r == CheckResult.FAILED && !checking) error else muted)
            background = GradientDrawable().apply {
                cornerRadius = px(22).toFloat()
                setColor(if (active) Color.rgb(20, 43, 34) else Color.rgb(15, 31, 25))
                setStroke(px(1), if (active) Color.argb(85, 124, 242, 177) else Color.argb(20, 124, 242, 177))
            }
            contentDescription = "${names[key]}. ${subtitle.text}. Нажмите для подробностей"
        }
    }

    private fun label(value: String, size: Float, color: Int) = TextView(context).apply { text = value; textSize = size; setTextColor(color) }
    private fun symbol(result: CheckResult) = when(result) { CheckResult.WORKS -> "✓"; CheckResult.ROUTE -> "↗"; CheckResult.FAILED -> "!"; CheckResult.FALLBACK -> "↪"; CheckResult.WAITING -> "…" }
    private fun px(value: Int) = (value * resources.displayMetrics.density).toInt()
    companion object {
        private val green = Color.rgb(124, 242, 177)
        private val white = Color.rgb(241, 247, 244)
        private val muted = Color.rgb(145, 166, 157)
        private val error = Color.rgb(255, 138, 138)
        private val amber = Color.rgb(246, 194, 112)
    }
}

/** A stateful emblem; animations never change connection state or run offscreen. */
internal class ArgusHeroView(context: Context) : View(context) {
    private val paint = Paint(Paint.ANTI_ALIAS_FLAG)
    private val shield = Path().apply {
        moveTo(54f,12f); cubicTo(42f,20f,30f,23f,20f,25f); lineTo(20f,50f)
        cubicTo(20f,72f,33f,88f,54f,96f); cubicTo(75f,88f,88f,72f,88f,50f)
        lineTo(88f,25f); cubicTo(78f,23f,66f,20f,54f,12f); close()
    }
    private val eye = Path().apply {
        moveTo(54f,29f); cubicTo(39f,29f,29f,40f,25f,51f)
        cubicTo(30f,62f,39f,73f,54f,73f); cubicTo(69f,73f,78f,62f,83f,51f)
        cubicTo(79f,40f,69f,29f,54f,29f); close()
    }
    private val ring = RectF()
    private var phase = 0f
    private var checking = false
    private var state = LightState()
    private var color = dormant
    private var animator: ValueAnimator? = null
    private var halo: RadialGradient? = null
    private var fire: SweepGradient? = null
    private var checks = List(8) { CheckResult.WAITING }

    init { isClickable = true; isFocusable = true; contentDescription = "Включить LIGHT" }
    fun render(value: LightState, busy: Boolean) {
        state = value
        checks = listOf(value.telegram.messages, value.youtube, value.whatsapp.messages,
            value.signal.messages, value.threema.messages, value.facebook.page, value.instagram.page, value.twitter.page)
        val nextColor = when {
            value.phase == LightPhase.ERROR -> coral
            busy || value.phase == LightPhase.LIMITED -> amber
            value.phase == LightPhase.ACTIVE -> mint
            else -> dormant
        }
        if (color != nextColor) { color = nextColor; updateShaders() }
        if (checking != busy) { checking = busy; updateAnimation() }
        invalidate()
    }
    private fun updateShaders() {
        val cx=width/2f; val cy=height/2f; val unit=minOf(width,height).toFloat()
        if (unit <= 0) return
        halo = RadialGradient(cx,cy,unit*.49f,intArrayOf(Color.argb(26,Color.red(color),Color.green(color),Color.blue(color)),Color.TRANSPARENT),null,Shader.TileMode.CLAMP)
        fire = SweepGradient(cx,cy,intArrayOf(Color.TRANSPARENT,Color.argb(12,255,173,75),Color.argb(120,255,167,67),Color.rgb(255,220,161),Color.TRANSPARENT),floatArrayOf(0f,.35f,.75f,.96f,1f))
    }
    private fun updateAnimation() {
        animator?.cancel(); animator = null
        if (checking && isAttachedToWindow && windowVisibility == VISIBLE && isShown && ValueAnimator.areAnimatorsEnabled()) {
            animator = ValueAnimator.ofFloat(0f,1f).apply {
                duration=2800; repeatCount=ValueAnimator.INFINITE
                interpolator=android.view.animation.LinearInterpolator()
                addUpdateListener { phase=it.animatedValue as Float; invalidate() }
                start()
            }
        } else { phase=0f; invalidate() }
    }
    override fun onSizeChanged(w:Int,h:Int,oldw:Int,oldh:Int) { super.onSizeChanged(w,h,oldw,oldh); updateShaders() }
    override fun onDraw(canvas: Canvas) {
        val cx=width/2f; val cy=height/2f; val unit=minOf(width,height).toFloat()
        paint.style=Paint.Style.FILL; paint.shader=halo
        canvas.drawCircle(cx,cy,unit*.49f,paint); paint.shader=null
        val radius=unit*.415f
        ring.set(cx-radius,cy-radius,cx+radius,cy+radius)
        paint.style=Paint.Style.STROKE; paint.strokeWidth=unit*.012f; paint.strokeCap=Paint.Cap.ROUND
        paint.color=Color.argb(27,Color.red(color),Color.green(color),Color.blue(color))
        canvas.drawCircle(cx,cy,radius,paint)
        if (checking) {
            val save=canvas.save(); canvas.rotate(phase*360f,cx,cy)
            paint.shader=fire; paint.strokeWidth=unit*.037f; paint.alpha=55
            canvas.drawArc(ring,0f,356f,false,paint)
            paint.strokeWidth=unit*.012f; paint.alpha=255
            canvas.drawArc(ring,0f,356f,false,paint); paint.shader=null
            canvas.restoreToCount(save)
            // Deterministic particles: no random flicker or per-frame allocation.
            for(i in 0 until 18) {
                val life=(phase*1.4f+i*.137f)%1f
                val angle=Math.toRadians((phase*360f+i*137.508f).toDouble())
                val r=radius+unit*.06f*life
                val x=cx+cos(angle).toFloat()*r; val y=cy+sin(angle).toFloat()*r
                paint.color=Color.argb(((1-life)*(1-life)*160).toInt(),255,205,134)
                paint.strokeWidth=unit*.004f
                canvas.drawLine(x,y,x+cos(angle).toFloat()*unit*.014f,y+sin(angle).toFloat()*unit*.014f,paint)
            }
        } else if (state.isRunning || state.phase == LightPhase.BASELINE) {
            paint.strokeWidth=unit*.015f
            checks.forEachIndexed { i,result ->
                paint.color=when(result) { CheckResult.WORKS->mint; CheckResult.FAILED->coral; CheckResult.ROUTE,CheckResult.FALLBACK->amber; else->Color.rgb(49,67,59) }
                canvas.drawArc(ring,-87f+i*45f,33f,false,paint)
            }
        }
        val save=canvas.save()
        val scale=unit*.81f/108f
        canvas.translate(cx-54*scale,cy-54*scale); canvas.scale(scale,scale)
        paint.style=Paint.Style.FILL; paint.color=color; canvas.drawPath(shield,paint)
        paint.color=Color.rgb(7,17,15); canvas.drawPath(eye,paint)
        paint.color=color; canvas.drawCircle(54f,51f,12f,paint)
        paint.color=Color.rgb(7,17,15); canvas.drawCircle(54f,51f,6f,paint)
        canvas.restoreToCount(save)
    }
    override fun performClick(): Boolean {
        if (ValueAnimator.areAnimatorsEnabled()) {
            animate().cancel(); scaleX=.96f; scaleY=.96f
            animate().scaleX(1f).scaleY(1f).setDuration(320).setInterpolator(PathInterpolator(.22f,1f,.36f,1f)).start()
        }
        return super.performClick()
    }
    override fun onAttachedToWindow() { super.onAttachedToWindow(); updateAnimation() }
    override fun onDetachedFromWindow() { animator?.cancel(); animator=null; animate().cancel(); super.onDetachedFromWindow() }
    override fun onWindowVisibilityChanged(visibility:Int) { super.onWindowVisibilityChanged(visibility); updateAnimation() }
    companion object {
        private val dormant=Color.rgb(126,151,140)
        private val mint=Color.rgb(124,242,177)
        private val amber=Color.rgb(246,194,112)
        private val coral=Color.rgb(255,138,138)
    }
}

/** Small recognisable service glyphs drawn locally; no remote assets or tracking. */
internal class ServiceGlyph(context: Context, private val service: String) : View(context) {
    var confirmed = false
        set(value) { field = value; invalidate() }
    private val paint = Paint(Paint.ANTI_ALIAS_FLAG)
    override fun onDraw(canvas: Canvas) {
        val save = canvas.save()
        canvas.scale(width / 48f, height / 48f)
        paint.style = Paint.Style.FILL
        paint.color = if (confirmed) Color.rgb(124, 242, 177) else when(service) {
            "telegram" -> Color.rgb(57, 174, 224)
            "youtube" -> Color.rgb(242, 81, 88)
            "whatsapp" -> Color.rgb(76, 199, 128)
            "signal" -> Color.rgb(96, 145, 241)
            "facebook" -> Color.rgb(63, 126, 232)
            "instagram" -> Color.rgb(205, 87, 144)
            "twitter" -> Color.rgb(46, 54, 50)
            else -> Color.rgb(211, 223, 218)
        }
        canvas.drawRoundRect(RectF(0f, 0f, 48f, 48f), 15f, 15f, paint)
        paint.color = if (confirmed || service == "threema") Color.rgb(7, 17, 15) else Color.WHITE
        val p = Path()
        when(service) {
            "facebook" -> {
                paint.typeface = Typeface.create("sans-serif", Typeface.BOLD); paint.textSize = 40f
                canvas.drawText("f",17f,41f,paint)
            }
            "instagram" -> {
                paint.style = Paint.Style.STROKE; paint.strokeWidth = 2.6f
                canvas.drawRoundRect(RectF(11f,11f,37f,37f),8f,8f,paint); canvas.drawCircle(24f,24f,6.5f,paint)
                paint.style = Paint.Style.FILL; canvas.drawCircle(32f,16.5f,1.9f,paint)
            }
            "twitter" -> {
                paint.style=Paint.Style.STROKE; paint.strokeWidth=2.4f
                p.moveTo(12f,12f); p.lineTo(20f,12f); p.lineTo(36f,36f); p.lineTo(28f,36f); p.close(); canvas.drawPath(p,paint); canvas.drawLine(35f,12f,12f,36f,paint); paint.style=Paint.Style.FILL
            }
            "telegram" -> { p.moveTo(10f, 22f); p.lineTo(37f, 11f); p.lineTo(31f, 37f); p.lineTo(22f, 29f); p.lineTo(17f, 34f); p.lineTo(18f, 26f); p.lineTo(30f, 17f); p.lineTo(15f, 25f); p.close(); canvas.drawPath(p, paint) }
            "youtube" -> { canvas.drawRoundRect(RectF(8f, 14f, 40f, 34f), 6f, 6f, paint); paint.color = if(confirmed) Color.rgb(124,242,177) else Color.rgb(242,81,88); p.moveTo(21f, 19f); p.lineTo(30f, 24f); p.lineTo(21f,29f); p.close(); canvas.drawPath(p,paint) }
            else -> {
                paint.style = Paint.Style.STROKE; paint.strokeWidth = 2f
                if(service == "signal") paint.pathEffect = android.graphics.DashPathEffect(floatArrayOf(3f,2f),0f)
                canvas.drawCircle(24f, 22f, 13f, paint)
                paint.pathEffect = null
                canvas.drawLine(14f,32f,12f,37f,paint); canvas.drawLine(12f,37f,21f,34f,paint)
                paint.style = Paint.Style.FILL
                if(service == "whatsapp") {
                    paint.style = Paint.Style.STROKE; paint.strokeWidth=4f; paint.strokeCap=Paint.Cap.ROUND
                    p.moveTo(19f,16f); p.cubicTo(15f,21f,25f,31f,30f,27f); canvas.drawPath(p,paint); paint.strokeCap=Paint.Cap.BUTT; paint.style=Paint.Style.FILL
                } else { for(x in listOf(18f,24f,30f)) canvas.drawCircle(x,22f,1.7f,paint) }
            }
        }
        canvas.restoreToCount(save)
    }
}
