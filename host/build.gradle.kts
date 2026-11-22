import org.jetbrains.kotlin.gradle.dsl.JvmTarget
import org.jetbrains.kotlin.gradle.tasks.KotlinJvmCompile

plugins {
    alias(libs.plugins.kotlin.jvm)
    alias(libs.plugins.kotlin.serialization)
    alias(libs.plugins.ktlint) apply false // lo aplican los módulos de AndroidCompat
    application
}

allprojects {
    group = "rensaigo"
    version = "0.1"
    repositories {
        // Los stubs de Android de Suwayomi se publican SIEMPRE como "1.0.0" y cambian de contenido (rama android-jar).
        // El 2026-11-07 se les quitaron clases que el tag v2.4.2366 aún necesita, así que se fijan por commit
        // (vendor.sh los descarga y comprueba su sha256) y se sirven desde un repositorio local exclusivo.
        exclusiveContent {
            forRepository { maven(rootProject.file("vendor/m2")) }
            filter { includeModule("com.github.Suwayomi", "android-jar") }
        }
        mavenCentral()
        google()
        maven("https://jitpack.io")
        maven("https://packages.jetbrains.team/maven/p/ij/intellij-dependencies")
    }
    plugins.withType<JavaPlugin> {
        extensions.configure<JavaPluginExtension> {
            sourceCompatibility = JavaVersion.VERSION_21
            targetCompatibility = JavaVersion.VERSION_21
        }
    }
    tasks.withType<KotlinJvmCompile> {
        compilerOptions {
            jvmTarget = JvmTarget.JVM_21
            freeCompilerArgs.add("-Xcontext-parameters")
        }
    }
}

// AndroidCompat se COMPILA con todas sus dependencias; solo se quitan del classpath de EJECUCIÓN del host.
// Dependencias de AndroidCompat que el host NO usa. Se excluyen de forma explícita (no se recorta ningún jar):
//  - GraalJS/Truffle: motor JavaScript (capa opcional: -PwithGraalJs)
//  - JCEF: Chromium embebido (WebView)
//  - dex2jar / apk-parser / axml: conversión de APK; Keiyoushi ya publica .jar
configurations.named("runtimeClasspath") {
    if (!providers.gradleProperty("withGraalJs").isPresent) {
        exclude(group = "org.graalvm.polyglot")
        exclude(group = "org.graalvm.js")
        exclude(group = "org.graalvm.truffle")
        exclude(group = "org.graalvm.regex")
        exclude(group = "org.graalvm.sdk")
        exclude(group = "org.graalvm.shadowed")
    }
    exclude(group = "org.jetbrains.intellij.deps.jcef")
    exclude(group = "de.femtopedia.dex2jar")
    exclude(group = "net.dongliu", module = "apk-parser")
    exclude(group = "com.github.Aliucord", module = "axml")
}

val vendorServer = "vendor/suwayomi/server/src/main/kotlin"

kotlin {
    sourceSets.main {
        // API de sources y red de Mihon tal cual la mantiene Suwayomi, SIN modificar. Lista explícita de lo que entra.
        kotlin.srcDir(vendorServer)
        kotlin.include(
            "rensaigo/**",
            "shim/**",
            "eu/kanade/tachiyomi/App.kt",
            "eu/kanade/tachiyomi/source/*.kt",
            "eu/kanade/tachiyomi/source/model/**",
            "eu/kanade/tachiyomi/source/online/**",
            "eu/kanade/tachiyomi/network/**",
            "eu/kanade/tachiyomi/util/JsoupExtensions.kt",
            "eu/kanade/tachiyomi/util/PkceUtil.kt",
            "eu/kanade/tachiyomi/util/chapter/**",
            "eu/kanade/tachiyomi/util/lang/CoroutinesExtensions.kt",
            "eu/kanade/tachiyomi/util/lang/Hash.kt",
            "suwayomi/tachidesk/manga/impl/util/lang/RxCoroutineBridge.kt",
            "suwayomi/tachidesk/manga/impl/util/lang/JsonObject.kt",
        )
    }
}

dependencies {
    implementation(project(":AndroidCompat"))
    implementation(project(":AndroidCompat:Config"))
    implementation(libs.android.stubs)

    implementation(libs.kotlin.stdlib.jdk8)
    implementation(libs.kotlin.reflect)
    implementation(libs.coroutines.core)
    implementation(libs.coroutines.jdk8)
    implementation(libs.serialization.json)
    implementation(libs.serialization.json.okio)
    implementation(libs.serialization.protobuf)
    implementation(libs.serialization.xml.core)
    implementation(libs.serialization.xml)

    implementation(libs.bundles.okhttp)
    implementation(libs.okio)
    implementation(libs.injekt)
    implementation(libs.koin.core)
    implementation(libs.rxjava)
    implementation(libs.jsoup)
    implementation(libs.config)
    // Android trae BouncyCastle de serie; algunas extensiones piden cifrados que el JDK no tiene (p. ej. AES/CBC/ZeroBytePadding)
    implementation(libs.bouncycastle)

    implementation(libs.slf4japi)
    implementation(libs.logback)
    implementation(libs.kotlinlogging)

    if (providers.gradleProperty("withRhino").isPresent) {
        implementation("org.mozilla:rhino:1.9.2")
    }
}

if (providers.gradleProperty("withRhino").isPresent) {
    // shim experimental de QuickJs sobre Rhino (ver src/rhino)
    sourceSets.main { java.srcDir("src/rhino/java") }
}

// Parche mínimo sobre AndroidCompat, aplicado al compilar (el código vendorizado no se toca):
// Paint.setFilterBitmap() lanza "Stub!" en Suwayomi v2.4.2366 y rompe el descifrado de imágenes del tema "catharsis".
// Solo es un indicador de calidad de escalado, así que basta con guardar el flag. La clase parcheada va en el jar del
// host, que precede a AndroidCompat en el classpath.
val patchAndroidCompat by tasks.registering {
    val src = file("vendor/suwayomi/AndroidCompat/src/main/java/android/graphics/Paint.java")
    val out = layout.buildDirectory.dir("generated/patched")
    inputs.file(src)
    outputs.dir(out)
    doLast {
        val stub = """    public void setFilterBitmap(boolean filter) {
        throw new RuntimeException("Stub!");
    }"""
        val fixed = """    public void setFilterBitmap(boolean filter) {
        int f = getFlags();
        setFlags(filter ? (f | FILTER_BITMAP_FLAG) : (f & ~FILTER_BITMAP_FLAG));
    }"""
        val text = src.readText()
        check(text.contains(stub)) { "Paint.java ha cambiado: revisar el parche de setFilterBitmap" }
        val dest = out.get().file("android/graphics/Paint.java").asFile
        dest.parentFile.mkdirs()
        dest.writeText(text.replace(stub, fixed))
    }
}
sourceSets.main { java.srcDir(patchAndroidCompat) }

application {
    mainClass.set("rensaigo.host.MainKt")
}

// El jar del host lleva el classpath ORDENADO en su manifiesto: AndroidCompat debe ir antes que los stubs de Android,
// porque sus clases android.* sustituyen a las de los stubs. Se ejecuta con: java -jar lib/rensaigo-host-0.1.jar
tasks.jar {
    manifest {
        attributes(
            "Main-Class" to "rensaigo.host.MainKt",
            "Class-Path" to configurations.runtimeClasspath.get().files.joinToString(" ") { it.name },
            "Enable-Native-Access" to "ALL-UNNAMED",
        )
    }
}
