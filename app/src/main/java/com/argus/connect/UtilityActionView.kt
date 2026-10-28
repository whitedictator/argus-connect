package com.argus.connect

import android.animation.ValueAnimator
import android.content.Context
import android.graphics.Canvas
import android.graphics.Color
import android.graphics.Paint
import android.graphics.Path
import android.graphics.RectF
import android.graphics.drawable.GradientDrawable
import android.view.View
import android.view.animation.PathInterpolator

/** Icon-only actions; names remain available to TalkBack and long-press hints. */
internal class UtilityActionView(context: Context, private val icon: String) : View(context) {
    var text: CharSequence = ""
        set(value) { field = value; contentDescription = value; tooltipText = value }
    var active = false
        set(value) { field = value; style(); invalidate() }
    private val paint = Paint(Paint.ANTI_ALIAS_FLAG)
    init { isClickable = true; isFocusable = true; style() }
    private fun style() {
        background = GradientDrawable().apply {
            cornerRadius = 18 * resources.displayMetrics.density
            setColor(if (active) Color.rgb(32, 66, 49) else Color.rgb(18, 35, 30))
        }
    }
    override fun onDraw(canvas: Canvas) {
        val unit = 28 * resources.displayMetrics.density
        val save = canvas.save()
        canvas.translate((width-unit)/2, (height-unit)/2)
        canvas.scale(unit/32, unit/32)
        paint.color = if (active) Color.rgb(124,242,177) else Color.rgb(208,224,215)
        paint.style = Paint.Style.STROKE; paint.strokeWidth = 1.8f; paint.strokeCap = Paint.Cap.ROUND; paint.strokeJoin = Paint.Join.ROUND
        val p = Path()
        when(icon) {
            "power" -> { canvas.drawArc(RectF(5f,5f,27f,27f), -52f, 284f, false, paint); canvas.drawLine(16f,3f,16f,16f,paint) }
            "hotspot" -> {
                for (radius in listOf(7f,12f)) { canvas.drawArc(RectF(16-radius,16-radius,16+radius,16+radius),135f,90f,false,paint); canvas.drawArc(RectF(16-radius,16-radius,16+radius,16+radius),-45f,90f,false,paint) }
                paint.style = Paint.Style.FILL; canvas.drawCircle(16f,16f,2.5f,paint)
            }
            "routes" -> {
                for ((x,y) in listOf(8f to 11f, 16f to 22f, 24f to 14f)) {
                    canvas.drawLine(x,4f,x,y-3f,paint); canvas.drawLine(x,y+3f,x,28f,paint)
                    canvas.drawCircle(x,y,3f,paint)
                }
            }
            else -> {
                p.moveTo(10f,4f); p.lineTo(22f,4f); p.lineTo(27f,9f); p.lineTo(27f,28f); p.lineTo(5f,28f); p.lineTo(5f,4f); p.lineTo(10f,4f); canvas.drawPath(p,paint)
                canvas.drawLine(16f,22f,16f,11f,paint); canvas.drawLine(11f,16f,16f,11f,paint); canvas.drawLine(16f,11f,21f,16f,paint)
            }
        }
        canvas.restoreToCount(save)
    }
    override fun performClick(): Boolean {
        if (ValueAnimator.areAnimatorsEnabled()) {
            animate().cancel(); scaleX=.92f; scaleY=.92f
            animate().scaleX(1f).scaleY(1f).setDuration(260).setInterpolator(PathInterpolator(.22f,1f,.36f,1f)).start()
        }
        return super.performClick()
    }
    override fun onDetachedFromWindow() { animate().cancel(); super.onDetachedFromWindow() }
}
