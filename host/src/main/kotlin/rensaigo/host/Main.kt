// rensaigo: host fino de extensiones Mihon/Keiyoushi.
// Protocolo: JSON por líneas en stdin/stdout (ver host/protocol.md). Los logs van a stderr.
package rensaigo.host

import android.os.Looper
import eu.kanade.tachiyomi.App
import eu.kanade.tachiyomi.network.HttpException
import eu.kanade.tachiyomi.network.JavaScriptEngine
import eu.kanade.tachiyomi.network.NetworkHelper
import eu.kanade.tachiyomi.source.CatalogueSource
import eu.kanade.tachiyomi.source.Source
import eu.kanade.tachiyomi.source.SourceFactory
import eu.kanade.tachiyomi.source.model.FilterList
import eu.kanade.tachiyomi.source.model.Page
import eu.kanade.tachiyomi.source.model.SChapter
import eu.kanade.tachiyomi.source.model.SManga
import eu.kanade.tachiyomi.source.online.HttpSource
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.launch
import kotlinx.serialization.json.Json
import kotlinx.serialization.json.JsonObject
import kotlinx.serialization.json.JsonObjectBuilder
import kotlinx.serialization.json.buildJsonArray
import kotlinx.serialization.json.buildJsonObject
import kotlinx.serialization.json.int
import kotlinx.serialization.json.intOrNull
import kotlinx.serialization.json.jsonObject
import kotlinx.serialization.json.jsonPrimitive
import kotlinx.serialization.json.put
import kotlinx.serialization.protobuf.ProtoBuf
import nl.adaptivity.xmlutil.serialization.XML
import org.koin.core.context.startKoin
import org.koin.dsl.module
import suwayomi.tachidesk.manga.impl.util.source.GetSource
import uy.kohesive.injekt.Injekt
import uy.kohesive.injekt.api.get
import xyz.nulldev.androidcompat.AndroidCompat
import xyz.nulldev.androidcompat.AndroidCompatInitializer
import xyz.nulldev.androidcompat.androidCompatModule
import xyz.nulldev.ts.config.configManagerModule
import java.io.File
import java.io.PrintStream
import java.net.URLClassLoader
import java.util.Locale
import java.util.concurrent.ConcurrentHashMap
import java.util.jar.JarFile
import kotlin.system.exitProcess

private val T0 = System.nanoTime()

private fun memStatus(): Pair<Long, Long> {
    var rss = 0L
    var hwm = 0L
    runCatching {
        File("/proc/self/status").forEachLine {
            if (it.startsWith("VmRSS:")) rss = it.split(Regex("\\s+"))[1].toLong()
            if (it.startsWith("VmHWM:")) hwm = it.split(Regex("\\s+"))[1].toLong()
        }
    }
    return rss to hwm
}

/** Extensiones cargadas: ruta del jar -> sources. Se recrean si cambia el User-Agent (tras resolver Cloudflare). */
private class Loaded(val jar: String, val loader: URLClassLoader, val classNames: List<String>, var sources: List<Source>)

private val loaded = ConcurrentHashMap<String, Loaded>()
private val sources = ConcurrentHashMap<Long, Source>()

/**
 * Android trae BouncyCastle de serie y algunas extensiones piden cifrados o rellenos que el JDK no tiene
 * (AES/CBC/ZeroBytePadding, PKCS7Padding…). Registrarlo siempre cuesta ~23 MB de RSS y ~430 ms (medido), y casi todas
 * las extensiones llevan javax.crypto en su bytecode aunque no lo usen. Así que se registra solo cuando una orden
 * falla porque falta un algoritmo, y esa orden se repite una vez.
 */
private val bcRegistered = java.util.concurrent.atomic.AtomicBoolean(false)

private fun missingCrypto(e: Throwable): Boolean =
    generateSequence(e) { it.cause }.take(8).any {
        it is java.security.NoSuchAlgorithmException || it is javax.crypto.NoSuchPaddingException || it is java.security.NoSuchProviderException ||
            (it.message ?: "").let { m -> "NoSuchAlgorithm" in m || "NoSuchPadding" in m || "Cannot find any provider" in m }
    }

private fun initAndroid() {
    Thread({
        @Suppress("DEPRECATION")
        Looper.prepareMainLooper()
        Looper.loop()
    }, "android-main").apply { isDaemon = true }.start()
    val app = App()
    startKoin {
        modules(
            module {
                single<android.app.Application> { app }
                single { app }
                single { NetworkHelper(app) }
                single { JavaScriptEngine(app) }
                single<Json> { Json { ignoreUnknownKeys = true; explicitNulls = false } }
                single<XML> { XML.v1 { policy { ignoreUnknownChildren(); autoPolymorphic = true } } }
                single<ProtoBuf> { ProtoBuf }
            },
            androidCompatModule(),
            configManagerModule(),
        )
    }
    AndroidCompatInitializer().init()
    AndroidCompat().startApp(app)
    // El cálculo del id de source depende de String.lowercase() con locale inglés (igual que Suwayomi).
    Locale.setDefault(Locale.ENGLISH)
    restoreUserAgent(Injekt.get<NetworkHelper>())
    System.setProperty("http.agent", Injekt.get<NetworkHelper>().defaultUserAgentProvider())
    GetSource.onUserAgentChanged = {
        loaded.values.forEach { l -> runCatching { l.sources = instantiate(l.loader, l.classNames); register(l) } }
    }
}

private fun instantiate(cl: ClassLoader, classNames: List<String>): List<Source> =
    classNames.flatMap { c ->
        when (val inst = Class.forName(c, false, cl).getDeclaredConstructor().newInstance()) {
            is Source -> listOf(inst)
            is SourceFactory -> inst.createSources()
            else -> error("clase de extensión desconocida: ${inst.javaClass}")
        }
    }

private fun register(l: Loaded) = l.sources.forEach { sources[it.id] = it }

private fun loadJar(path: String): Loaded {
    loaded[path]?.let { return it }
    val f = File(path)
    val manifest = JarFile(f).use { jf ->
        val e = jf.getEntry("AndroidManifest.xml") ?: error("el jar no contiene AndroidManifest.xml")
        jf.getInputStream(e).readBytes().decodeToString()
    }
    val pkg = Regex("package=\"([^\"]+)\"").find(manifest)?.groupValues?.get(1) ?: error("manifest sin package")
    val cls = Regex("android:name=\"tachiyomi.extension.class\"\\s+android:value=\"([^\"]+)\"").find(manifest)?.groupValues?.get(1)
        ?: error("manifest sin tachiyomi.extension.class")
    val names = cls.split(";").map { it.trim() }.filter { it.isNotEmpty() }.map { if (it.startsWith(".")) pkg + it else it }
    val cl = URLClassLoader(arrayOf(f.toURI().toURL()), Source::class.java.classLoader)
    val l = Loaded(path, cl, names, instantiate(cl, names))
    loaded[path] = l
    register(l)
    return l
}

private fun prefScreen(s: Source): androidx.preference.PreferenceScreen? {
    val cs = s as? eu.kanade.tachiyomi.source.ConfigurableSource ?: return null
    val screen = androidx.preference.PreferenceScreen(Injekt.get<android.app.Application>())
    screen.sharedPreferences = Injekt.get<android.app.Application>().getSharedPreferences("source_${s.id}", android.content.Context.MODE_PRIVATE)
    cs.setupPreferenceScreen(screen)
    return screen
}

private fun prefValue(v: Any?): kotlinx.serialization.json.JsonElement = when (v) {
    null -> kotlinx.serialization.json.JsonNull
    is Boolean -> kotlinx.serialization.json.JsonPrimitive(v)
    is Number -> kotlinx.serialization.json.JsonPrimitive(v)
    is Collection<*> -> buildJsonArray { v.forEach { add(kotlinx.serialization.json.JsonPrimitive(it.toString())) } }
    else -> kotlinx.serialization.json.JsonPrimitive(v.toString())
}

private fun JsonObject.str(k: String): String? = this[k]?.jsonPrimitive?.takeIf { it.isString || it.content != "null" }?.content

private fun mangaJson(m: SManga, full: Boolean = false) = buildJsonObject {
    put("url", m.url)
    put("title", m.title)
    put("thumbnail", m.thumbnail_url)
    // memo (extension-lib 1.6): estado opaco de la extensión que hay que devolverle tal cual en las órdenes siguientes
    runCatching { m.memo }.getOrNull()?.takeIf { it.isNotEmpty() }?.let { put("memo", it) }
    if (full) {
        put("author", m.author)
        put("artist", m.artist)
        put("description", m.description)
        put("genre", m.genre)
        put("status", m.status)
    }
}

private fun mangaOf(o: JsonObject): SManga = SManga.create().apply {
    url = o.str("url") ?: error("manga.url requerido")
    title = o.str("title") ?: ""
    thumbnail_url = o.str("thumbnail")
    (o["memo"] as? JsonObject)?.let { memo = it }
}

private fun src(req: JsonObject): Source =
    sources[(req.str("source") ?: error("source requerido")).toLong()] ?: error("source no cargado: ${req.str("source")}")

private suspend fun handle(req: JsonObject): JsonObject = when (val op = req.str("op") ?: error("op requerido")) {
    "ping" -> buildJsonObject { put("pong", true) }
    "load" -> buildJsonObject {
        val l = loadJar(req.str("jar") ?: error("jar requerido"))
        put("sources", buildJsonArray {
            l.sources.forEach {
                add(buildJsonObject {
                    put("id", it.id.toString())
                    put("name", it.name)
                    put("lang", it.lang)
                    put("baseUrl", (it as? HttpSource)?.baseUrl)
                    put("supportsLatest", (it as? CatalogueSource)?.supportsLatest ?: false)
                })
            }
        })
    }
    "popular", "latest", "search" -> {
        val s = src(req)
        val page = req["page"]?.jsonPrimitive?.intOrNull ?: 1
        val mp = when (op) {
            "popular" -> s.getPopularManga(page)
            "latest" -> s.getLatestUpdates(page)
            else -> s.getSearchManga(page, req.str("query") ?: "", (s as? CatalogueSource)?.getFilterList() ?: FilterList())
        }
        buildJsonObject {
            put("hasNext", mp.hasNextPage)
            put("mangas", buildJsonArray { mp.mangas.forEach { add(mangaJson(it)) } })
        }
    }
    "details" -> {
        val s = src(req)
        val m = mangaOf(req["manga"]!!.jsonObject)
        val wantChapters = req["chapters"]?.jsonPrimitive?.content != "false"
        val u = s.getMangaUpdate(m, emptyList(), true, wantChapters)
        // Las extensiones suelen devolver un SManga nuevo solo con los detalles: url y título (lateinit) pueden faltar.
        val um = u.manga
        if (runCatching { um.url }.isFailure) um.url = m.url
        if (runCatching { um.title }.getOrNull().isNullOrBlank()) um.title = m.title
        buildJsonObject {
            put("manga", mangaJson(um, full = true))
            put("webUrl", runCatching { (s as? HttpSource)?.getMangaUrl(m) }.getOrNull())
            put("chapters", buildJsonArray {
                u.chapters.forEach { c ->
                    add(buildJsonObject {
                        put("url", c.url)
                        put("name", c.name)
                        put("number", c.chapter_number)
                        put("date", c.date_upload)
                        put("scanlator", c.scanlator)
                        runCatching { c.memo }.getOrNull()?.takeIf { it.isNotEmpty() }?.let { put("memo", it) }
                        put("webUrl", runCatching { (s as? HttpSource)?.getChapterUrl(c) }.getOrNull())
                    })
                }
            })
        }
    }
    "pages" -> {
        val s = src(req)
        val c = req["chapter"]!!.jsonObject
        val ch = SChapter.create().apply {
            url = c.str("url") ?: error("chapter.url requerido")
            name = c.str("name") ?: ""
            c.str("scanlator")?.let { scanlator = it }
            c["number"]?.jsonPrimitive?.content?.toFloatOrNull()?.let { chapter_number = it }
            c["date"]?.jsonPrimitive?.content?.toLongOrNull()?.let { date_upload = it }
            (c["memo"] as? JsonObject)?.let { memo = it }
        }
        val pages = s.getPageList(ch)
        buildJsonObject {
            put("pages", buildJsonArray {
                pages.forEachIndexed { i, p -> add(buildJsonObject { put("index", i); put("url", p.url); put("imageUrl", p.imageUrl) }) }
            })
        }
    }
    "image" -> {
        // La imagen se descarga SIEMPRE con el cliente de la extensión (cabeceras, cookies, interceptores de descifrado).
        val s = src(req) as? HttpSource ?: error("el source no es HTTP")
        val p = req["page"]!!.jsonObject
        val page = Page(p["index"]?.jsonPrimitive?.int ?: 0, p.str("url") ?: "", p.str("imageUrl"))
        if (page.imageUrl.isNullOrEmpty()) page.imageUrl = s.getImageUrl(page)
        val out = File(req.str("out") ?: error("out requerido"))
        val tmp = File(out.path + ".part")
        val resp = s.getImage(page)
        val n = resp.use { r -> tmp.outputStream().use { o -> r.body.byteStream().copyTo(o) } }
        if (!tmp.renameTo(out)) error("no se pudo mover ${tmp.path}")
        buildJsonObject { put("bytes", n); put("contentType", resp.header("Content-Type")); put("imageUrl", page.imageUrl) }
    }
    "prefs" -> {
        // Preferencias de la extensión (ConfigurableSource): las declara ella misma sobre una pantalla simulada.
        val s = src(req)
        buildJsonObject {
            put("prefs", buildJsonArray {
                prefScreen(s)?.preferences?.forEachIndexed { i, p ->
                    add(buildJsonObject {
                        put("index", i); put("key", p.key); put("title", p.title?.toString()); put("summary", runCatching { p.summary?.toString() }.getOrNull())
                        put("kind", p.javaClass.simpleName); put("valueType", p.defaultValueType)
                        put("enabled", p.isEnabled); put("visible", p.visible)
                        put("value", prefValue(runCatching { p.currentValue }.getOrNull()))
                        put("default", prefValue(p.defaultValue))
                        val (entries, values) = when (p) {
                            is androidx.preference.ListPreference -> p.entries to p.entryValues
                            is androidx.preference.MultiSelectListPreference -> p.entries to p.entryValues
                            else -> null to null
                        }
                        if (entries != null && values != null) {
                            put("entries", buildJsonArray { entries.forEach { add(kotlinx.serialization.json.JsonPrimitive(it.toString())) } })
                            put("entryValues", buildJsonArray { values.forEach { add(kotlinx.serialization.json.JsonPrimitive(it.toString())) } })
                        }
                    })
                }
            })
        }
    }
    "setpref" -> {
        val s = src(req)
        val screen = prefScreen(s) ?: error("el source no tiene preferencias")
        val key = req.str("key")
        val pref = screen.preferences.firstOrNull { it.key == key } ?: error("preferencia desconocida: $key")
        if (!pref.isEnabled) error("preferencia desactivada")
        val raw = req.str("value") ?: ""
        val value: Any = when (pref.defaultValueType) {
            "String" -> raw
            "Boolean" -> raw.toBoolean()
            "Set<String>" -> Json.parseToJsonElement(raw.ifBlank { "[]" }).let { e -> (e as kotlinx.serialization.json.JsonArray).map { it.jsonPrimitive.content }.toSet() }
            else -> error("tipo de preferencia no soportado: ${pref.defaultValueType}")
        }
        pref.saveNewValue(value)
        pref.callChangeListener(value)
        // la extensión lee sus preferencias al crearse: se vuelve a instanciar
        loaded.values.filter { l -> l.sources.any { it.id == s.id } }.forEach { l -> l.sources = instantiate(l.loader, l.classNames); register(l) }
        buildJsonObject { put("saved", true) }
    }
    "mem" -> {
        if (req["gc"]?.jsonPrimitive?.content == "true") System.gc()
        val (r, h) = memStatus()
        val rt = Runtime.getRuntime()
        buildJsonObject {
            put("rss_kb", r); put("hwm_kb", h)
            put("heap_used_kb", (rt.totalMemory() - rt.freeMemory()) / 1024); put("heap_committed_kb", rt.totalMemory() / 1024)
            put("loaded_jars", loaded.size); put("sources", sources.size)
        }
    }
    "cookies" -> buildJsonObject {
        // Diagnóstico: nombres de cookies guardadas por dominio (sin valores).
        val store = Injekt.get<NetworkHelper>().cookieStore
        put("cookies", buildJsonArray {
            store.getStoredCookies().forEach { c -> add(buildJsonObject { put("domain", c.domain); put("name", c.name); put("expiresAt", c.expiresAt) }) }
        })
        put("userAgent", Injekt.get<NetworkHelper>().defaultUserAgentProvider())
    }
    else -> error("op desconocida: $op")
}

// --- Persistencia de lo que resuelve FlareSolverr -------------------------------------------------------------
// Dos fallos del código de Suwayomi (v2.4.2366) hacían que CADA arranque del host repitiera el desafío de Cloudflare:
//  1. FlareSolverr 3.x devuelve la caducidad de las cookies en el campo "expiry", pero el modelo lee "expires":
//     cf_clearance queda como cookie de sesión y PersistentCookieStore no la escribe en disco.
//  2. cf_clearance solo vale con el User-Agent con el que se obtuvo, y ese User-Agent vive solo en memoria.
// Se corrige desde fuera, sin tocar el código vendorizado.
private val uaFile by lazy { File(xyz.nulldev.ts.config.ApplicationRootDir, "user_agent.txt") }
private val CF_COOKIES = setOf("cf_clearance")

@Suppress("UNCHECKED_CAST")
private fun restoreUserAgent(net: NetworkHelper) {
    val saved = runCatching { uaFile.readText().trim() }.getOrNull()?.takeIf { it.isNotEmpty() } ?: return
    runCatching {
        val f = NetworkHelper::class.java.getDeclaredField("userAgent").apply { isAccessible = true }
        (f.get(net) as kotlinx.coroutines.flow.MutableStateFlow<String>).value = saved
    }.onFailure { System.err.println("no se pudo restaurar el User-Agent: $it") }
}

/** Convierte en persistentes las cf_clearance de sesión (30 días) y guarda el User-Agent vigente. */
private fun persistClearance() {
    val net = Injekt.get<NetworkHelper>()
    val pending = net.cookieStore.getStoredCookies().filter { it.name in CF_COOKIES && !it.persistent }
    if (pending.isEmpty()) return
    val until = System.currentTimeMillis() + 30L * 24 * 3600 * 1000
    pending.groupBy { it.domain }.forEach { (domain, cs) ->
        val url = okhttp3.HttpUrl.Builder().scheme("https").host(domain).build()
        net.cookieStore.addAll(url, cs.map { it.newBuilder().expiresAt(until).build() })
    }
    runCatching { uaFile.writeText(net.defaultUserAgentProvider()) }
}

/** Clasifica el fallo para que el núcleo pueda mostrar el estado del source. */
private fun errorJson(e: Throwable): JsonObject {
    val chain = generateSequence(e) { it.cause }.take(8).toList()
    val text = chain.joinToString(" | ") { it.toString() }
    val http = chain.filterIsInstance<HttpException>().firstOrNull()?.code
        ?: Regex("HTTP error (\\d{3})").find(text)?.groupValues?.get(1)?.toIntOrNull()
    val frames = chain.flatMap { it.stackTrace.take(4).toList() }.joinToString(" ") { it.className }
    val kind = when {
        // Turnstile de Cloudflare: la extensión intenta abrir una vista de navegador (context.resources es nulo en AndroidCompat)
        "TurnstileHelper" in frames || "Resources.getConfiguration" in text -> "webview"
        // API de Android que AndroidCompat no implementa (p. ej. PdfRenderer para capítulos en PDF)
        "Stub!" in text -> "android"
        "org/cef" in text || "org.cef" in text || "WebView" in text || "Kcef" in text -> "webview"
        "org/graalvm" in text || "org.graalvm" in text || "QuickJs" in text || "polyglot" in text -> "javascript"
        "Cloudflare" in text || "cloudflare" in text -> "cloudflare"
        http != null -> "http"
        chain.any { it is java.net.UnknownHostException || it is java.net.SocketTimeoutException || it is java.net.ConnectException || it is javax.net.ssl.SSLException } -> "network"
        else -> "extension"
    }
    return buildJsonObject {
        put("kind", kind)
        put("message", text.take(600))
        if (http != null) put("http", http)
        put("trace", e.stackTrace.take(5).joinToString(" | "))
    }
}

fun main() {
    val out = PrintStream(java.io.FileOutputStream(java.io.FileDescriptor.out), false, "UTF-8")
    System.setOut(System.err) // nada que imprima una extensión puede ensuciar el protocolo
    val lock = Any()
    fun send(o: JsonObject) = synchronized(lock) { out.println(o.toString()); out.flush() }

    try {
        initAndroid()
    } catch (e: Throwable) {
        send(buildJsonObject { put("event", "fatal"); put("error", errorJson(e)) })
        exitProcess(2)
    }
    val (r, h) = memStatus()
    send(buildJsonObject {
        put("event", "ready"); put("protocol", 1)
        put("init_ms", (System.nanoTime() - T0) / 1_000_000)
        put("jvm_uptime_ms", java.lang.management.ManagementFactory.getRuntimeMXBean().uptime)
        put("rss_kb", r); put("hwm_kb", h)
        put("flaresolverr", suwayomi.tachidesk.server.serverConfig.flareSolverrEnabled.value)
    })

    val json = Json { ignoreUnknownKeys = true }
    val scope = CoroutineScope(SupervisorJob() + Dispatchers.IO)
    while (true) {
        val line = readlnOrNull() ?: break // stdin cerrado: el núcleo ha muerto o nos apaga
        if (line.isBlank()) continue
        val req = try {
            json.parseToJsonElement(line).jsonObject
        } catch (e: Exception) {
            send(buildJsonObject { put("ok", false); put("error", errorJson(e)) })
            continue
        }
        val id = req["id"]
        if (req.str("op") == "quit") break
        // Cada orden corre en su corrutina: el núcleo decide la concurrencia.
        scope.launch {
            val t = System.nanoTime()
            val res = try {
                val result = try {
                    handle(req)
                } catch (e: Throwable) {
                    if (!missingCrypto(e)) throw e
                    if (bcRegistered.compareAndSet(false, true)) {
                        java.security.Security.addProvider(org.bouncycastle.jce.provider.BouncyCastleProvider())
                    }
                    handle(req) // segunda oportunidad, ya con BouncyCastle
                }
                buildJsonObject { put("ok", true); put("result", result) }
            } catch (e: Throwable) {
                if (System.getenv("RENSAIGO_HOST_TRACE") != null) e.printStackTrace() // diagnóstico: traza completa a stderr
                buildJsonObject { put("ok", false); put("error", errorJson(e)) }
            }
            runCatching { persistClearance() }
            send(JsonObject(res + buildJsonObject { if (id != null) put("id", id); put("ms", (System.nanoTime() - t) / 1_000_000) }))
        }
    }
    exitProcess(0)
}
