// Piezas mínimas que el código de Suwayomi (sin modificar) espera encontrar en el servidor completo.
// El host no tiene ServerConfig, base de datos ni registro de sources: aquí van sustitutos de pocas líneas.
@file:Suppress("ktlint")

package suwayomi.tachidesk.server

import kotlinx.coroutines.flow.MutableStateFlow

/** Sustituye a ServerConfig: solo los ajustes de FlareSolverr que lee CloudflareInterceptor. */
object HostServerConfig {
    private fun env(name: String, def: String) = System.getenv(name)?.takeIf { it.isNotBlank() } ?: def

    val flareSolverrUrl = MutableStateFlow(env("RENSAIGO_FLARESOLVERR_URL", ""))
    val flareSolverrEnabled = MutableStateFlow(flareSolverrUrl.value.isNotBlank())
    val flareSolverrTimeout = MutableStateFlow(env("RENSAIGO_FLARESOLVERR_TIMEOUT", "60").toInt())
    val flareSolverrSessionName = MutableStateFlow(env("RENSAIGO_FLARESOLVERR_SESSION", "rensaigo"))
    val flareSolverrSessionTtl = MutableStateFlow(env("RENSAIGO_FLARESOLVERR_SESSION_TTL", "15").toInt())
    val flareSolverrAsResponseFallback = MutableStateFlow(false)
}

val serverConfig = HostServerConfig
