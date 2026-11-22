@file:Suppress("ktlint")

package suwayomi.tachidesk.manga.impl.util.source

/** NetworkHelper llama a esto cuando cambia el User-Agent (tras resolver Cloudflare). El host recrea los sources. */
object GetSource {
    @Volatile var onUserAgentChanged: () -> Unit = {}

    fun unregisterAllSources() = onUserAgentChanged()
}
