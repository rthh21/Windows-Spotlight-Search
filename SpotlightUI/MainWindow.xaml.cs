using System;
using System.Collections.Concurrent;
using System.Collections.Generic;
using System.Diagnostics;
using System.IO;
using System.Net.Http;
using System.Net.Http.Json;
using System.Net.Sockets;
using System.Runtime.InteropServices;
using System.Text.Json;
using System.Text.Json.Serialization;
using System.Threading;
using System.Threading.Tasks;
using System.Windows;
using System.Windows.Controls;
using System.Windows.Input;
using System.Windows.Interop;
using System.Windows.Media;
using System.Windows.Media.Imaging;

namespace SpotlightUI
{
    public static class IconHelper
    {
        [StructLayout(LayoutKind.Sequential, CharSet = CharSet.Unicode)]
        private struct SHFILEINFO
        {
            public IntPtr hIcon;
            public int iIcon;
            public uint dwAttributes;
            [MarshalAs(UnmanagedType.ByValTStr, SizeConst = 260)]
            public string szDisplayName;
            [MarshalAs(UnmanagedType.ByValTStr, SizeConst = 80)]
            public string szTypeName;
        }

        [DllImport("shell32.dll", CharSet = CharSet.Unicode)]
        private static extern IntPtr SHGetFileInfo(string pszPath, uint dwFileAttributes, ref SHFILEINFO psfi, uint cbFileInfo, uint uFlags);

        [DllImport("user32.dll", SetLastError = true)]
        private static extern bool DestroyIcon(IntPtr hIcon);

        [DllImport("shell32.dll", CharSet = CharSet.Unicode, PreserveSig = false)]
        private static extern void SHCreateItemFromParsingName(
            [MarshalAs(UnmanagedType.LPWStr)] string pszPath,
            IntPtr pbc,
            [MarshalAs(UnmanagedType.LPStruct)] Guid riid,
            [MarshalAs(UnmanagedType.Interface)] out IShellItemImageFactory ppv);

        [ComImport]
        [Guid("bcc18b79-ba16-442f-80c4-8a59c30c463b")]
        [InterfaceType(ComInterfaceType.InterfaceIsIUnknown)]
        private interface IShellItemImageFactory
        {
            [PreserveSig]
            int GetImage(SIZE size, int flags, out IntPtr phbm);
        }

        [StructLayout(LayoutKind.Sequential)]
        private struct SIZE
        {
            public int cx;
            public int cy;
            public SIZE(int cx, int cy) { this.cx = cx; this.cy = cy; }
        }

        [DllImport("gdi32.dll", SetLastError = true)]
        [return: MarshalAs(UnmanagedType.Bool)]
        private static extern bool DeleteObject(IntPtr hObject);

        private const uint SHGFI_ICON = 0x000000100;
        private const uint SHGFI_LARGEICON = 0x000000000;
        private const uint SHGFI_USEFILEATTRIBUTES = 0x000000010;
        private const uint FILE_ATTRIBUTE_NORMAL = 0x00000080;
        private const uint FILE_ATTRIBUTE_DIRECTORY = 0x00000010;

        private static readonly ConcurrentDictionary<string, ImageSource?> _cache = new(StringComparer.OrdinalIgnoreCase);

        [DllImport("shell32.dll", CharSet = CharSet.Auto)]
        private static extern uint ExtractIconEx(string szFileName, int nIconIndex, IntPtr[]? phiconLarge, IntPtr[]? phiconSmall, uint nIcons);

        public static ImageSource? GetIcon(string path, string extension)
        {
            if (string.IsNullOrEmpty(path) || path.StartsWith("cmd:") || extension == "CALC" || extension == "SYS" || extension == "WEB")
                return null;

            // Extract native high-resolution icon for Windows Store / UWP apps from shell:AppsFolder
            if (path.StartsWith("shell:AppsFolder", StringComparison.OrdinalIgnoreCase))
            {
                if (_cache.TryGetValue(path, out var cachedApp))
                    return cachedApp;

                try
                {
                    Guid guid = new Guid("bcc18b79-ba16-442f-80c4-8a59c30c463b");
                    SHCreateItemFromParsingName(path, IntPtr.Zero, guid, out var factory);
                    if (factory != null)
                    {
                        IntPtr hBitmap = IntPtr.Zero;
                        if (factory.GetImage(new SIZE(32, 32), 0x00, out hBitmap) != 0 || hBitmap == IntPtr.Zero)
                        {
                            factory.GetImage(new SIZE(32, 32), 0x02, out hBitmap); // SIIGBF_ICONONLY
                        }
                        if (hBitmap == IntPtr.Zero)
                        {
                            factory.GetImage(new SIZE(32, 32), 0x01, out hBitmap); // SIIGBF_BIGGERSIZEOK
                        }
                        if (hBitmap == IntPtr.Zero)
                        {
                            factory.GetImage(new SIZE(32, 32), 0x100, out hBitmap); // SIIGBF_SCALEUP
                        }

                        if (hBitmap != IntPtr.Zero)
                        {
                            try
                            {
                                var bs = Imaging.CreateBitmapSourceFromHBitmap(hBitmap, IntPtr.Zero, Int32Rect.Empty, BitmapSizeOptions.FromEmptyOptions());
                                bs.Freeze();
                                _cache[path] = bs;
                                return bs;
                            }
                            finally
                            {
                                DeleteObject(hBitmap);
                            }
                        }
                    }
                }
                catch { }

                return GetDefaultAppIcon();
            }

            // Suport pentru comenzi steam://rungameid/<id>
            if (path.StartsWith("steam://", StringComparison.OrdinalIgnoreCase))
            {
                if (_cache.TryGetValue(path, out var cachedSteam))
                    return cachedSteam;

                var steamGameIcon = ResolveSteamProtocolIcon(path);
                if (steamGameIcon != null)
                {
                    _cache[path] = steamGameIcon;
                    return steamGameIcon;
                }
            }

            string resolvedPath = path;
            if (!File.Exists(resolvedPath) && !resolvedPath.Contains('\\') && !resolvedPath.Contains('/'))
            {
                string sys32 = Path.Combine(Environment.SystemDirectory, resolvedPath);
                if (File.Exists(sys32))
                {
                    resolvedPath = sys32;
                }
                else
                {
                    string winDir = Path.Combine(Environment.GetFolderPath(Environment.SpecialFolder.Windows), resolvedPath);
                    if (File.Exists(winDir))
                    {
                        resolvedPath = winDir;
                    }
                }
            }

            bool isApp = extension.Equals(".exe", StringComparison.OrdinalIgnoreCase) ||
                         extension.Equals(".lnk", StringComparison.OrdinalIgnoreCase) ||
                         extension.Equals(".url", StringComparison.OrdinalIgnoreCase) ||
                         extension.Equals("APP", StringComparison.OrdinalIgnoreCase) ||
                         resolvedPath.EndsWith(".exe", StringComparison.OrdinalIgnoreCase) ||
                         resolvedPath.EndsWith(".lnk", StringComparison.OrdinalIgnoreCase) ||
                         resolvedPath.EndsWith(".url", StringComparison.OrdinalIgnoreCase);

            bool isFolder = extension == "DIR" || (!resolvedPath.Contains('.') && Directory.Exists(resolvedPath));

            string cacheKey = isApp
                ? resolvedPath
                : (isFolder ? "__FOLDER__" : ("ext:" + (string.IsNullOrEmpty(extension) ? "FILE" : extension.ToLowerInvariant())));

            if (_cache.TryGetValue(cacheKey, out var cached))
                return cached;

            try
            {
                if (isFolder)
                {
                    SHFILEINFO sfi = new();
                    if (SHGetFileInfo("folder", FILE_ATTRIBUTE_DIRECTORY, ref sfi, (uint)Marshal.SizeOf<SHFILEINFO>(), SHGFI_USEFILEATTRIBUTES | SHGFI_ICON | SHGFI_LARGEICON) != IntPtr.Zero && sfi.hIcon != IntPtr.Zero)
                    {
                        try
                        {
                            var bs = Imaging.CreateBitmapSourceFromHIcon(sfi.hIcon, Int32Rect.Empty, BitmapSizeOptions.FromEmptyOptions());
                            bs.Freeze();
                            _cache[cacheKey] = bs;
                            return bs;
                        }
                        finally
                        {
                            DestroyIcon(sfi.hIcon);
                        }
                    }
                }
                else if (resolvedPath.EndsWith(".url", StringComparison.OrdinalIgnoreCase))
                {
                    var urlIcon = ExtractUrlIcon(resolvedPath);
                    if (urlIcon != null)
                    {
                        _cache[cacheKey] = urlIcon;
                        return urlIcon;
                    }
                }
                else if (resolvedPath.EndsWith(".lnk", StringComparison.OrdinalIgnoreCase))
                {
                    if (File.Exists(resolvedPath))
                    {
                        try
                        {
                            using var sysIcon = System.Drawing.Icon.ExtractAssociatedIcon(resolvedPath);
                            if (sysIcon != null)
                            {
                                var bs = Imaging.CreateBitmapSourceFromHIcon(sysIcon.Handle, Int32Rect.Empty, BitmapSizeOptions.FromEmptyOptions());
                                bs.Freeze();
                                _cache[cacheKey] = bs;
                                return bs;
                            }
                        }
                        catch { }

                        SHFILEINFO sfi = new();
                        if (SHGetFileInfo(resolvedPath, 0, ref sfi, (uint)Marshal.SizeOf<SHFILEINFO>(), SHGFI_ICON | SHGFI_LARGEICON) != IntPtr.Zero && sfi.hIcon != IntPtr.Zero)
                        {
                            try
                            {
                                var bs = Imaging.CreateBitmapSourceFromHIcon(sfi.hIcon, Int32Rect.Empty, BitmapSizeOptions.FromEmptyOptions());
                                bs.Freeze();
                                _cache[cacheKey] = bs;
                                return bs;
                            }
                            finally
                            {
                                DestroyIcon(sfi.hIcon);
                            }
                        }
                    }
                }
                else if (isApp)
                {
                    if (File.Exists(resolvedPath))
                    {
                        SHFILEINFO sfi = new();
                        if (SHGetFileInfo(resolvedPath, 0, ref sfi, (uint)Marshal.SizeOf<SHFILEINFO>(), SHGFI_ICON | SHGFI_LARGEICON) != IntPtr.Zero && sfi.hIcon != IntPtr.Zero)
                        {
                            try
                            {
                                var bs = Imaging.CreateBitmapSourceFromHIcon(sfi.hIcon, Int32Rect.Empty, BitmapSizeOptions.FromEmptyOptions());
                                bs.Freeze();
                                _cache[cacheKey] = bs;
                                return bs;
                            }
                            finally
                            {
                                DestroyIcon(sfi.hIcon);
                            }
                        }

                        try
                        {
                            using var sysIcon = System.Drawing.Icon.ExtractAssociatedIcon(resolvedPath);
                            if (sysIcon != null)
                            {
                                var bs = Imaging.CreateBitmapSourceFromHIcon(sysIcon.Handle, Int32Rect.Empty, BitmapSizeOptions.FromEmptyOptions());
                                bs.Freeze();
                                _cache[cacheKey] = bs;
                                return bs;
                            }
                        }
                        catch { }
                    }

                    // Fallback pentru orice aplicație: iconița standard .exe
                    var defaultApp = GetDefaultAppIcon();
                    if (defaultApp != null)
                    {
                        _cache[cacheKey] = defaultApp;
                        return defaultApp;
                    }
                }
                else
                {
                    // Fișier obișnuit (.pdf, .txt, etc.)
                    if (File.Exists(resolvedPath))
                    {
                        SHFILEINFO sfi = new();
                        if (SHGetFileInfo(resolvedPath, 0, ref sfi, (uint)Marshal.SizeOf<SHFILEINFO>(), SHGFI_ICON | SHGFI_LARGEICON) != IntPtr.Zero && sfi.hIcon != IntPtr.Zero)
                        {
                            try
                            {
                                var bs = Imaging.CreateBitmapSourceFromHIcon(sfi.hIcon, Int32Rect.Empty, BitmapSizeOptions.FromEmptyOptions());
                                bs.Freeze();
                                _cache[cacheKey] = bs;
                                return bs;
                            }
                            finally
                            {
                                DestroyIcon(sfi.hIcon);
                            }
                        }
                    }

                    string queryExt = extension.StartsWith('.') ? extension : ("." + extension);
                    SHFILEINFO sfiExt = new();
                    if (SHGetFileInfo(queryExt, FILE_ATTRIBUTE_NORMAL, ref sfiExt, (uint)Marshal.SizeOf<SHFILEINFO>(), SHGFI_USEFILEATTRIBUTES | SHGFI_ICON | SHGFI_LARGEICON) != IntPtr.Zero && sfiExt.hIcon != IntPtr.Zero)
                    {
                        try
                        {
                            var bs = Imaging.CreateBitmapSourceFromHIcon(sfiExt.hIcon, Int32Rect.Empty, BitmapSizeOptions.FromEmptyOptions());
                            bs.Freeze();
                            _cache[cacheKey] = bs;
                            return bs;
                        }
                        finally
                        {
                            DestroyIcon(sfiExt.hIcon);
                        }
                    }
                }
            }
            catch { }

            if (isApp)
            {
                return GetDefaultAppIcon();
            }

            return null;
        }

        public static ImageSource? ExtractUrlIcon(string urlPath)
        {
            try
            {
                if (!File.Exists(urlPath)) return null;

                string? iconFile = null;
                int iconIndex = 0;
                string? targetUrl = null;

                foreach (var line in File.ReadAllLines(urlPath))
                {
                    var trimmed = line.Trim();
                    if (trimmed.StartsWith("IconFile=", StringComparison.OrdinalIgnoreCase))
                    {
                        iconFile = trimmed.Substring(9).Trim('\"', ' ');
                    }
                    else if (trimmed.StartsWith("IconIndex=", StringComparison.OrdinalIgnoreCase))
                    {
                        int.TryParse(trimmed.Substring(10).Trim(), out iconIndex);
                    }
                    else if (trimmed.StartsWith("URL=", StringComparison.OrdinalIgnoreCase))
                    {
                        targetUrl = trimmed.Substring(4).Trim();
                    }
                }

                if (!string.IsNullOrEmpty(iconFile) && File.Exists(iconFile))
                {
                    if (iconFile.EndsWith(".ico", StringComparison.OrdinalIgnoreCase))
                    {
                        try
                        {
                            using var fs = new FileStream(iconFile, FileMode.Open, FileAccess.Read, FileShare.Read);
                            var decoder = BitmapDecoder.Create(fs, BitmapCreateOptions.None, BitmapCacheOption.OnLoad);
                            if (decoder.Frames.Count > 0)
                            {
                                BitmapFrame bestFrame = decoder.Frames[0];
                                foreach (var frame in decoder.Frames)
                                {
                                    if (frame.PixelWidth >= 32 && frame.PixelWidth <= 64)
                                    {
                                        bestFrame = frame;
                                        break;
                                    }
                                }
                                bestFrame.Freeze();
                                return bestFrame;
                            }
                        }
                        catch { }
                    }
                    else
                    {
                        try
                        {
                            using var sys = System.Drawing.Icon.ExtractAssociatedIcon(iconFile);
                            if (sys != null)
                            {
                                var bs = Imaging.CreateBitmapSourceFromHIcon(sys.Handle, Int32Rect.Empty, BitmapSizeOptions.FromEmptyOptions());
                                bs.Freeze();
                                return bs;
                            }
                        }
                        catch { }
                    }
                }

                // Încercăm SHGetFileInfo pe fișierul .url
                SHFILEINFO sfi = new();
                if (SHGetFileInfo(urlPath, 0, ref sfi, (uint)Marshal.SizeOf<SHFILEINFO>(), SHGFI_ICON | SHGFI_LARGEICON) != IntPtr.Zero && sfi.hIcon != IntPtr.Zero)
                {
                    try
                    {
                        var bs = Imaging.CreateBitmapSourceFromHIcon(sfi.hIcon, Int32Rect.Empty, BitmapSizeOptions.FromEmptyOptions());
                        bs.Freeze();
                        return bs;
                    }
                    finally
                    {
                        DestroyIcon(sfi.hIcon);
                    }
                }

                // Fallback pentru jocuri Steam sau Epic
                if (!string.IsNullOrEmpty(targetUrl))
                {
                    if (targetUrl.StartsWith("steam://", StringComparison.OrdinalIgnoreCase))
                    {
                        string steamExe = @"C:\Program Files (x86)\Steam\steam.exe";
                        if (File.Exists(steamExe))
                        {
                            using var sys = System.Drawing.Icon.ExtractAssociatedIcon(steamExe);
                            if (sys != null)
                            {
                                var bs = Imaging.CreateBitmapSourceFromHIcon(sys.Handle, Int32Rect.Empty, BitmapSizeOptions.FromEmptyOptions());
                                bs.Freeze();
                                return bs;
                            }
                        }
                    }
                }
            }
            catch { }
            return null;
        }

        private static ImageSource? ResolveSteamProtocolIcon(string steamUrl)
        {
            try
            {
                string appId = steamUrl.Substring("steam://".Length).Trim();
                if (appId.StartsWith("rungameid/", StringComparison.OrdinalIgnoreCase))
                {
                    appId = appId.Substring("rungameid/".Length).Trim();
                }

                // Căutăm pe Desktop sau Start Menu
                string[] searchDirs = new[]
                {
                    Environment.GetFolderPath(Environment.SpecialFolder.Desktop),
                    Path.Combine(Environment.GetFolderPath(Environment.SpecialFolder.ApplicationData), @"Microsoft\Windows\Start Menu\Programs\Steam"),
                    Path.Combine(Environment.GetFolderPath(Environment.SpecialFolder.CommonApplicationData), @"Microsoft\Windows\Start Menu\Programs\Steam")
                };

                foreach (var dir in searchDirs)
                {
                    if (Directory.Exists(dir))
                    {
                        foreach (var urlFile in Directory.GetFiles(dir, "*.url", SearchOption.TopDirectoryOnly))
                        {
                            try
                            {
                                var text = File.ReadAllText(urlFile);
                                if (text.Contains(appId, StringComparison.OrdinalIgnoreCase))
                                {
                                    var icon = ExtractUrlIcon(urlFile);
                                    if (icon != null) return icon;
                                }
                            }
                            catch { }
                        }
                    }
                }

                string steamGames = @"C:\Program Files (x86)\Steam\steam\games";
                if (Directory.Exists(steamGames))
                {
                    var icos = Directory.GetFiles(steamGames, "*.ico");
                    if (icos.Length > 0)
                    {
                        using var fs = new FileStream(icos[0], FileMode.Open, FileAccess.Read, FileShare.Read);
                        var decoder = BitmapDecoder.Create(fs, BitmapCreateOptions.None, BitmapCacheOption.OnLoad);
                        if (decoder.Frames.Count > 0)
                        {
                            var f = decoder.Frames[0];
                            f.Freeze();
                            return f;
                        }
                    }
                }

                string steamExe = @"C:\Program Files (x86)\Steam\steam.exe";
                if (File.Exists(steamExe))
                {
                    using var sys = System.Drawing.Icon.ExtractAssociatedIcon(steamExe);
                    if (sys != null)
                    {
                        var bs = Imaging.CreateBitmapSourceFromHIcon(sys.Handle, Int32Rect.Empty, BitmapSizeOptions.FromEmptyOptions());
                        bs.Freeze();
                        return bs;
                    }
                }
            }
            catch { }
            return null;
        }

        private static ImageSource? GetDefaultAppIcon()
        {
            if (_cache.TryGetValue("__DEFAULT_APP__", out var cached) && cached != null)
                return cached;

            try
            {
                SHFILEINFO sfi = new();
                if (SHGetFileInfo(".exe", FILE_ATTRIBUTE_NORMAL, ref sfi, (uint)Marshal.SizeOf<SHFILEINFO>(), SHGFI_USEFILEATTRIBUTES | SHGFI_ICON | SHGFI_LARGEICON) != IntPtr.Zero && sfi.hIcon != IntPtr.Zero)
                {
                    try
                    {
                        var bs = Imaging.CreateBitmapSourceFromHIcon(sfi.hIcon, Int32Rect.Empty, BitmapSizeOptions.FromEmptyOptions());
                        bs.Freeze();
                        _cache["__DEFAULT_APP__"] = bs;
                        return bs;
                    }
                    finally
                    {
                        DestroyIcon(sfi.hIcon);
                    }
                }
            }
            catch { }
            return null;
        }
    }

    public class FileResult
    {
        [JsonPropertyName("name")]
        public string Name { get; set; } = "";

        [JsonPropertyName("path")]
        public string Path { get; set; } = "";

        [JsonPropertyName("ext")]
        public string Extension { get; set; } = "";

        [JsonPropertyName("category")]
        public string Category { get; set; } = "";

        [JsonPropertyName("score")]
        public int Score { get; set; }

        public string DisplayExt => string.IsNullOrEmpty(Extension) ? "FILE" : Extension.TrimStart('.').ToUpper();

        [JsonIgnore]
        public string Subtitle
        {
            get
            {
                if (Extension == "CALC") return "Calculator • Press Enter to copy result";
                if (Path.StartsWith("cmd:copy:")) return "Action • Press Enter to copy";
                if (Path == "cmd:settings") return "Settings • Spotlight shortcuts and commands";
                if (Path.StartsWith("cmd:")) return "System Action • " + Category;
                if (Extension == "WEB") return "Web • Press Enter to search";
                if (Extension == "APP") return "Application";
                if (Extension == "DIR") return "Folder • " + Path;
                return (string.IsNullOrEmpty(Category) ? "File" : Category) + " • " + Path;
            }
        }

        [JsonIgnore]
        public string BadgeIcon
        {
            get
            {
                return Extension switch
                {
                    "CALC" => "=",
                    "SYS" => "⚡",
                    "APP" => "◆",
                    "WEB" => "🌐",
                    "DIR" => "📁",
                    _ => DisplayExt.Length > 4 ? DisplayExt.Substring(0, 4) : DisplayExt
                };
            }
        }

        private static readonly SolidColorBrush BrushCalc = new(Color.FromRgb(48, 209, 88));   // Green Calculator
        private static readonly SolidColorBrush BrushSys = new(Color.FromRgb(255, 159, 10));  // Orange System
        private static readonly SolidColorBrush BrushApp = new(Color.FromRgb(10, 132, 255));  // Blue Application
        private static readonly SolidColorBrush BrushDir = new(Color.FromRgb(255, 214, 10));  // Gold Folder
        private static readonly SolidColorBrush BrushWeb = new(Color.FromRgb(0, 168, 232));   // Cyan Web
        private static readonly SolidColorBrush BrushDefault = new(Color.FromRgb(58, 58, 60)); // Neutral File

        private static readonly SolidColorBrush TagBgApp = new(Color.FromArgb(40, 10, 132, 255));
        private static readonly SolidColorBrush TagBorderApp = new(Color.FromArgb(100, 10, 132, 255));
        private static readonly SolidColorBrush TagBgAction = new(Color.FromArgb(40, 255, 159, 10));
        private static readonly SolidColorBrush TagBorderAction = new(Color.FromArgb(100, 255, 159, 10));
        private static readonly SolidColorBrush TagBgCalc = new(Color.FromArgb(40, 48, 209, 88));
        private static readonly SolidColorBrush TagBorderCalc = new(Color.FromArgb(100, 48, 209, 88));
        private static readonly SolidColorBrush TagBgWeb = new(Color.FromArgb(40, 0, 168, 232));
        private static readonly SolidColorBrush TagBorderWeb = new(Color.FromArgb(100, 0, 168, 232));
        private static readonly SolidColorBrush TagBgDir = new(Color.FromArgb(40, 255, 214, 10));
        private static readonly SolidColorBrush TagBorderDir = new(Color.FromArgb(100, 255, 214, 10));
        private static readonly SolidColorBrush TagBgFile = new(Color.FromArgb(30, 255, 255, 255));
        private static readonly SolidColorBrush TagBorderFile = new(Color.FromArgb(60, 255, 255, 255));

        static FileResult()
        {
            BrushCalc.Freeze();
            BrushSys.Freeze();
            BrushApp.Freeze();
            BrushDir.Freeze();
            BrushWeb.Freeze();
            BrushDefault.Freeze();
            TagBgApp.Freeze();
            TagBorderApp.Freeze();
            TagBgAction.Freeze();
            TagBorderAction.Freeze();
            TagBgCalc.Freeze();
            TagBorderCalc.Freeze();
            TagBgWeb.Freeze();
            TagBorderWeb.Freeze();
            TagBgDir.Freeze();
            TagBorderDir.Freeze();
            TagBgFile.Freeze();
            TagBorderFile.Freeze();
        }

        [JsonIgnore]
        public Brush BadgeBackgroundBrush
        {
            get
            {
                return Extension switch
                {
                    "CALC" => BrushCalc,
                    "SYS" => BrushSys,
                    "APP" => BrushApp,
                    "WEB" => BrushWeb,
                    "DIR" => BrushDir,
                    _ => BrushDefault
                };
            }
        }

        [JsonIgnore]
        public bool IsTopHit => string.Equals(Category, "Top Hit", StringComparison.OrdinalIgnoreCase);

        [JsonIgnore]
        public Visibility TopHitVisibility => IsTopHit ? Visibility.Visible : Visibility.Collapsed;

        [JsonIgnore]
        public string CategoryBadgeText
        {
            get
            {
                if (IsTopHit) return "";
                if (Extension == "APP") return "APPLICATION";
                if (Extension == "SYS") return "ACTION";
                if (Extension == "CALC") return "CALCULATOR";
                if (Extension == "WEB") return "WEB";
                if (Extension == "DIR") return "FOLDER";
                return "FILE";
            }
        }

        [JsonIgnore]
        public Visibility CategoryBadgeVisibility => string.IsNullOrEmpty(CategoryBadgeText) ? Visibility.Collapsed : Visibility.Visible;

        [JsonIgnore]
        public Brush CategoryBadgeBrush
        {
            get
            {
                return Extension switch
                {
                    "APP" => TagBgApp,
                    "SYS" => TagBgAction,
                    "CALC" => TagBgCalc,
                    "WEB" => TagBgWeb,
                    "DIR" => TagBgDir,
                    _ => TagBgFile
                };
            }
        }

        [JsonIgnore]
        public Brush CategoryBadgeBorderBrush
        {
            get
            {
                return Extension switch
                {
                    "APP" => TagBorderApp,
                    "SYS" => TagBorderAction,
                    "CALC" => TagBorderCalc,
                    "WEB" => TagBorderWeb,
                    "DIR" => TagBorderDir,
                    _ => TagBorderFile
                };
            }
        }

        [JsonIgnore]
        public ImageSource? Icon => IconHelper.GetIcon(Path, Extension);

        [JsonIgnore]
        public Visibility IconVisibilityReal => Icon != null ? Visibility.Visible : Visibility.Collapsed;

        [JsonIgnore]
        public Visibility IconVisibilityFallback => Icon == null ? Visibility.Visible : Visibility.Collapsed;
    }

    public class TcpSearchResponse
    {
        [JsonPropertyName("query")]
        public string Query { get; set; } = "";

        [JsonPropertyName("latency_ms")]
        public double LatencyMs { get; set; }

        [JsonPropertyName("count")]
        public int Count { get; set; }

        [JsonPropertyName("results")]
        public List<FileResult> Results { get; set; } = new();
    }

    public partial class MainWindow : Window
    {
        private static readonly HttpClient _http = new() { BaseAddress = new Uri("http://127.0.0.1:9876"), Timeout = TimeSpan.FromSeconds(2) };
        private CancellationTokenSource? _searchCts;
        private readonly CancellationTokenSource _appLifetimeCts = new();

        // Socket TCP Persistent pentru latență < 1ms
        private TcpClient? _tcpClient;
        private NetworkStream? _tcpStream;
        private StreamReader? _tcpReader;
        private StreamWriter? _tcpWriter;
        private readonly SemaphoreSlim _tcpLock = new(1, 1);
        private bool _isTcpConnecting;

        // Win32 Global Hotkeys (Alt + Space, Ctrl + Space, Alt + Enter)
        private const int HOTKEY_ALT_SPACE = 9000;
        private const int HOTKEY_CTRL_SPACE = 9001;
        private const int HOTKEY_ALT_ENTER = 9002;
        private const uint MOD_ALT = 0x0001;
        private const uint MOD_CONTROL = 0x0002;
        private const uint VK_SPACE = 0x20;
        private const uint VK_RETURN = 0x0D;
        private const int WM_HOTKEY = 0x0312;

        [DllImport("user32.dll")]
        private static extern bool RegisterHotKey(IntPtr hWnd, int id, uint fsModifiers, uint vk);

        [DllImport("user32.dll")]
        private static extern bool UnregisterHotKey(IntPtr hWnd, int id);

        [DllImport("user32.dll")]
        private static extern bool SetForegroundWindow(IntPtr hWnd);

        [StructLayout(LayoutKind.Sequential)]
        private struct POINT
        {
            public int X;
            public int Y;
        }

        [StructLayout(LayoutKind.Sequential)]
        private struct RECT
        {
            public int Left;
            public int Top;
            public int Right;
            public int Bottom;
        }

        [StructLayout(LayoutKind.Sequential, CharSet = CharSet.Auto)]
        private struct MONITORINFO
        {
            public int cbSize;
            public RECT rcMonitor;
            public RECT rcWork;
            public int dwFlags;
        }

        private const uint MONITOR_DEFAULTTONEAREST = 0x00000002;

        [DllImport("user32.dll")]
        private static extern bool GetCursorPos(out POINT lpPoint);

        [DllImport("user32.dll")]
        private static extern IntPtr MonitorFromPoint(POINT pt, uint dwFlags);

        [DllImport("user32.dll")]
        private static extern IntPtr MonitorFromWindow(IntPtr hwnd, uint dwFlags);

        [DllImport("user32.dll", CharSet = CharSet.Auto)]
        private static extern bool GetMonitorInfo(IntPtr hMonitor, ref MONITORINFO lpmi);

        [DllImport("shcore.dll")]
        private static extern int GetDpiForMonitor(IntPtr hMonitor, int dpiType, out uint dpiX, out uint dpiY);

        public MainWindow()
        {
            InitializeComponent();

            PositionOnCurrentScreen();

            Loaded += (s, e) =>
            {
                UpdateCustomCaretPosition();
                StartCaretBlink();
            };

            EnsureBackendRunning();
            _ = InitializeTcpClientAsync();
        }

        protected override void OnSourceInitialized(EventArgs e)
        {
            base.OnSourceInitialized(e);
            IntPtr handle = new WindowInteropHelper(this).Handle;
            HwndSource.FromHwnd(handle)?.AddHook(HwndHook);

            RegisterHotKey(handle, HOTKEY_ALT_SPACE, MOD_ALT, VK_SPACE);
            RegisterHotKey(handle, HOTKEY_CTRL_SPACE, MOD_CONTROL, VK_SPACE);
            RegisterHotKey(handle, HOTKEY_ALT_ENTER, MOD_ALT, VK_RETURN);
        }

        private IntPtr HwndHook(IntPtr hwnd, int msg, IntPtr wParam, IntPtr lParam, ref bool handled)
        {
            if (msg == WM_HOTKEY)
            {
                int id = wParam.ToInt32();
                if (id == HOTKEY_ALT_SPACE || id == HOTKEY_CTRL_SPACE || id == HOTKEY_ALT_ENTER)
                {
                    ToggleSpotlight();
                    handled = true;
                }
            }
            return IntPtr.Zero;
        }

        // ====================================================================
        // BACKEND MANAGEMENT & PERSISTENT TCP PROTOCOL
        // ====================================================================
        private static void EnsureBackendRunning()
        {
            try
            {
                // Check if backend is already running on port 49152
                using (var testClient = new TcpClient())
                {
                    var ar = testClient.BeginConnect("127.0.0.1", 49152, null, null);
                    if (ar.AsyncWaitHandle.WaitOne(150))
                    {
                        testClient.EndConnect(ar);
                        return; // Already running
                    }
                }
            }
            catch { }

            // Search for spotlight-backend.exe binary
            string baseDir = AppDomain.CurrentDomain.BaseDirectory;
            string[] candidates = new[]
            {
                Path.Combine(baseDir, "spotlight-backend.exe"),
                Path.GetFullPath(Path.Combine(baseDir, @"..\..\..\..\spotlight-backend\spotlight-backend.exe")),
                Path.GetFullPath(Path.Combine(baseDir, @"..\..\..\spotlight-backend\spotlight-backend.exe")),
                @"C:\Users\rth\Documents\Cod\Spotlight\spotlight-backend\spotlight-backend.exe"
            };

            foreach (var path in candidates)
            {
                if (File.Exists(path))
                {
                    try
                    {
                        var psi = new ProcessStartInfo(path)
                        {
                            CreateNoWindow = true,
                            WindowStyle = ProcessWindowStyle.Hidden,
                            UseShellExecute = false,
                            WorkingDirectory = Path.GetDirectoryName(path)
                        };
                        Process.Start(psi);
                        break;
                    }
                    catch { }
                }
            }
        }

        private async Task InitializeTcpClientAsync()
        {
            if (_isTcpConnecting) return;
            _isTcpConnecting = true;

            try
            {
                for (int attempt = 0; attempt < 5 && !_appLifetimeCts.IsCancellationRequested; attempt++)
                {
                    try
                    {
                        var client = new TcpClient();
                        client.NoDelay = true; // Sub-millisecond latency
                        await client.ConnectAsync("127.0.0.1", 49152, _appLifetimeCts.Token);

                        var stream = client.GetStream();
                        var reader = new StreamReader(stream);
                        var writer = new StreamWriter(stream) { AutoFlush = true };

                        await _tcpLock.WaitAsync();
                        try
                        {
                            _tcpClient?.Dispose();
                            _tcpClient = client;
                            _tcpStream = stream;
                            _tcpReader = reader;
                            _tcpWriter = writer;
                        }
                        finally
                        {
                            _tcpLock.Release();
                        }

                        _ = Task.Run(() => ListenTcpLoopAsync(_appLifetimeCts.Token));
                        return;
                    }
                    catch
                    {
                        await Task.Delay(400, _appLifetimeCts.Token);
                    }
                }
            }
            catch { }
            finally
            {
                _isTcpConnecting = false;
            }
        }

        private async Task ListenTcpLoopAsync(CancellationToken token)
        {
            try
            {
                while (!token.IsCancellationRequested)
                {
                    StreamReader? reader;
                    await _tcpLock.WaitAsync(token);
                    try
                    {
                        reader = _tcpReader;
                    }
                    finally
                    {
                        _tcpLock.Release();
                    }

                    if (reader == null) break;

                    string? line = await reader.ReadLineAsync(token);
                    if (line == null) break;

                    line = line.Trim();
                    if (line.StartsWith("{\"status\":") || line.Length == 0) continue;

                    try
                    {
                        var response = JsonSerializer.Deserialize<TcpSearchResponse>(line);
                        if (response != null)
                        {
                            // Preload icons pe firul de fundal astfel încât firul UI să nu aibă nicio întârziere
                            foreach (var r in response.Results)
                            {
                                _ = r.Icon;
                            }

                            Dispatcher.Invoke(() =>
                            {
                                string currentText = SearchBox.Text.Trim();
                                if (string.Equals(response.Query, currentText, StringComparison.OrdinalIgnoreCase))
                                {
                                    UpdateResultsUI(response.Results, response.LatencyMs);
                                }
                            });
                        }
                    }
                    catch { }
                }
            }
            catch { }
            finally
            {
                await _tcpLock.WaitAsync();
                try
                {
                    _tcpWriter = null;
                    _tcpReader?.Dispose();
                    _tcpReader = null;
                    _tcpStream?.Dispose();
                    _tcpStream = null;
                    _tcpClient?.Dispose();
                    _tcpClient = null;
                }
                finally
                {
                    _tcpLock.Release();
                }

                if (!_appLifetimeCts.IsCancellationRequested)
                {
                    _ = Task.Delay(1000).ContinueWith(_ => InitializeTcpClientAsync());
                }
            }
        }

        // ====================================================================
        // SEARCH & UI DISPATCH
        // ====================================================================
        private async void SearchBox_TextChanged(object sender, TextChangedEventArgs e)
        {
            string query = SearchBox.Text;
            PlaceholderText.Visibility = string.IsNullOrEmpty(query) ? Visibility.Visible : Visibility.Collapsed;
            ClearButton.Visibility = string.IsNullOrEmpty(query) ? Visibility.Collapsed : Visibility.Visible;

            UpdateCustomCaretPosition();

            if (SettingsOverlay.Visibility == Visibility.Visible && !query.Equals(":setari", StringComparison.OrdinalIgnoreCase) && !query.Equals(":settings", StringComparison.OrdinalIgnoreCase))
            {
                SettingsOverlay.Visibility = Visibility.Collapsed;
            }

            _searchCts?.Cancel();
            if (string.IsNullOrWhiteSpace(query))
            {
                UpdateResultsUI(new List<FileResult>(), 0);
                return;
            }

            if (query.Trim().Equals(":setari", StringComparison.OrdinalIgnoreCase))
            {
                ShowSettings();
                return;
            }

            _searchCts = new CancellationTokenSource();
            var token = _searchCts.Token;

            try
            {
                // Debounce ultra-rapid de 10ms (reacție instantanee la tastare)
                await Task.Delay(10, token);

                string trimmedQuery = query.Trim();
                bool sentViaTcp = false;

                await _tcpLock.WaitAsync(token);
                try
                {
                    if (_tcpWriter != null && _tcpClient != null && _tcpClient.Connected)
                    {
                        // Protocol TCP: "limit|query\n"
                        await _tcpWriter.WriteLineAsync($"24|{trimmedQuery}");
                        sentViaTcp = true;
                    }
                }
                catch { }
                finally
                {
                    _tcpLock.Release();
                }

                // Fallback HTTP dacă TCP nu este disponibil
                if (!sentViaTcp)
                {
                    var sw = Stopwatch.StartNew();
                    var results = await _http.GetFromJsonAsync<List<FileResult>>(
                        $"/search?q={Uri.EscapeDataString(trimmedQuery)}&limit=24", token);
                    sw.Stop();

                    if (!token.IsCancellationRequested && results != null)
                    {
                        foreach (var r in results)
                        {
                            _ = r.Icon;
                        }
                        UpdateResultsUI(results, sw.Elapsed.TotalMilliseconds);
                    }
                }
            }
            catch (TaskCanceledException) { }
            catch (Exception) { }
        }

        private void UpdateResultsUI(List<FileResult> results, double latencyMs)
        {
            if (results.Count > 0)
            {
                ResultsList.ItemsSource = results;
                ResultsList.SelectedIndex = 0;
                ResultsList.Visibility = Visibility.Visible;
                SeparatorLine.Visibility = Visibility.Visible;
            }
            else
            {
                ResultsList.ItemsSource = null;
                ResultsList.Visibility = Visibility.Collapsed;
                SeparatorLine.Visibility = Visibility.Collapsed;
            }
        }

        // ====================================================================
        // LAUNCH AND COMMAND EXECUTION
        // ====================================================================
        private void LaunchItem(FileResult item)
        {
            try
            {
                if (string.IsNullOrWhiteSpace(item.Path))
                    return;

                if (item.Path.StartsWith("cmd:copy:"))
                {
                    string textToCopy = item.Path.Substring("cmd:copy:".Length);
                    Clipboard.SetDataObject(textToCopy, true);
                    HideSpotlight();
                    SearchBox.Clear();
                    return;
                }

                if (item.Path == "cmd:settings")
                {
                    ShowSettings();
                    return;
                }

                if (item.Path.StartsWith("cmd:"))
                {
                    _ = ExecuteBackendActionAsync(item.Path);
                    HideSpotlight();
                    SearchBox.Clear();
                    return;
                }

                HideSpotlight();
                SearchBox.Clear();
                Process.Start(new ProcessStartInfo(item.Path) { UseShellExecute = true });
            }
            catch (Exception ex)
            {
                MessageBox.Show($"Could not open: {ex.Message}", "Spotlight", MessageBoxButton.OK, MessageBoxImage.Warning);
            }
        }

        private async Task ExecuteBackendActionAsync(string command)
        {
            try
            {
                await _tcpLock.WaitAsync();
                try
                {
                    if (_tcpWriter != null && _tcpClient != null && _tcpClient.Connected)
                    {
                        await _tcpWriter.WriteLineAsync(command);
                        return;
                    }
                }
                finally
                {
                    _tcpLock.Release();
                }

                await _http.GetAsync($"/action?run={Uri.EscapeDataString(command)}");
            }
            catch { }
        }

        // ====================================================================
        // KEYBOARD & MOUSE EVENTS
        // ====================================================================
        private void Window_PreviewKeyDown(object sender, KeyEventArgs e)
        {
            if (e.Key == Key.Escape)
            {
                if (SettingsOverlay.Visibility == Visibility.Visible)
                {
                    SettingsOverlay.Visibility = Visibility.Collapsed;
                }
                else
                {
                    HideSpotlight();
                }
                e.Handled = true;
            }
            else if (e.Key == Key.Down && ResultsList.Items.Count > 0)
            {
                if (ResultsList.SelectedIndex < ResultsList.Items.Count - 1)
                {
                    ResultsList.SelectedIndex++;
                    ResultsList.ScrollIntoView(ResultsList.SelectedItem);
                }
                e.Handled = true;
            }
            else if (e.Key == Key.Up && ResultsList.Items.Count > 0)
            {
                if (ResultsList.SelectedIndex > 0)
                {
                    ResultsList.SelectedIndex--;
                    ResultsList.ScrollIntoView(ResultsList.SelectedItem);
                }
                e.Handled = true;
            }
            else if (e.Key == Key.Enter)
            {
                ExecuteSearchAction();
                e.Handled = true;
            }
        }

        private void ResultsList_MouseDoubleClick(object sender, MouseButtonEventArgs e)
        {
            if (ResultsList.SelectedItem is FileResult selected)
            {
                LaunchItem(selected);
            }
        }

        private void MenuOpenLocation_Click(object sender, RoutedEventArgs e)
        {
            if (ResultsList.SelectedItem is FileResult item && !string.IsNullOrEmpty(item.Path) && !item.Path.StartsWith("cmd:"))
            {
                try
                {
                    Process.Start("explorer.exe", $"/select,\"{item.Path}\"");
                    HideSpotlight();
                }
                catch { }
            }
        }

        private void MenuCopyPath_Click(object sender, RoutedEventArgs e)
        {
            if (ResultsList.SelectedItem is FileResult item && !string.IsNullOrEmpty(item.Path))
            {
                string text = item.Path.StartsWith("cmd:copy:") ? item.Path.Substring("cmd:copy:".Length) : item.Path;
                Clipboard.SetDataObject(text, true);
            }
        }

        private void ClearButton_Click(object sender, RoutedEventArgs e)
        {
            SearchBox.Clear();
            SearchBox.Focus();
        }

        private void SettingsButton_Click(object sender, RoutedEventArgs e)
        {
            ExecuteSearchAction();
        }

        private void ExecuteSearchAction()
        {
            if (ResultsList.Items.Count > 0)
            {
                if (ResultsList.SelectedItem is FileResult selected)
                {
                    LaunchItem(selected);
                    return;
                }
                else if (ResultsList.Items[0] is FileResult first)
                {
                    LaunchItem(first);
                    return;
                }
            }

            string query = SearchBox.Text?.Trim() ?? string.Empty;
            if (!string.IsNullOrWhiteSpace(query))
            {
                try
                {
                    if (query.StartsWith("http://", StringComparison.OrdinalIgnoreCase) || query.StartsWith("https://", StringComparison.OrdinalIgnoreCase))
                    {
                        Process.Start(new ProcessStartInfo(query) { UseShellExecute = true });
                    }
                    else
                    {
                        Process.Start(new ProcessStartInfo($"https://www.google.com/search?q={Uri.EscapeDataString(query)}") { UseShellExecute = true });
                    }
                    HideSpotlight();
                    SearchBox.Clear();
                }
                catch { }
            }
            else
            {
                SearchBox.Focus();
            }
        }

        private void CloseSettings_Click(object sender, RoutedEventArgs e)
        {
            SettingsOverlay.Visibility = Visibility.Collapsed;
            SearchBox.Focus();
        }

        private void ShowSettings()
        {
            SettingsOverlay.Visibility = Visibility.Visible;
            ResultsList.Visibility = Visibility.Collapsed;
            SeparatorLine.Visibility = Visibility.Collapsed;
        }

        private void ToggleSettings()
        {
            if (SettingsOverlay.Visibility == Visibility.Visible)
            {
                SettingsOverlay.Visibility = Visibility.Collapsed;
                SearchBox.Focus();
            }
            else
            {
                ShowSettings();
            }
        }

        private void PositionOnCurrentScreen()
        {
            try
            {
                if (!GetCursorPos(out POINT cursorPt))
                    return;

                IntPtr hMonitor = MonitorFromPoint(cursorPt, MONITOR_DEFAULTTONEAREST);
                if (hMonitor == IntPtr.Zero)
                    return;

                var mi = new MONITORINFO();
                mi.cbSize = Marshal.SizeOf<MONITORINFO>();
                if (!GetMonitorInfo(hMonitor, ref mi))
                    return;

                uint dpiX = 96, dpiY = 96;
                try
                {
                    GetDpiForMonitor(hMonitor, 0, out dpiX, out dpiY);
                }
                catch
                {
                    var dpi = VisualTreeHelper.GetDpi(this);
                    dpiX = (uint)(dpi.PixelsPerInchX > 0 ? dpi.PixelsPerInchX : 96);
                    dpiY = (uint)(dpi.PixelsPerInchY > 0 ? dpi.PixelsPerInchY : 96);
                }

                double scaleX = dpiX > 0 ? (dpiX / 96.0) : 1.0;
                double scaleY = dpiY > 0 ? (dpiY / 96.0) : 1.0;

                double workLeft = mi.rcWork.Left / scaleX;
                double workTop = mi.rcWork.Top / scaleY;
                double workWidth = (mi.rcWork.Right - mi.rcWork.Left) / scaleX;
                double workHeight = (mi.rcWork.Bottom - mi.rcWork.Top) / scaleY;

                double windowWidth = double.IsNaN(Width) || Width <= 0 ? 680 : Width;

                Left = workLeft + (workWidth - windowWidth) / 2.0;
                Top = workTop + (workHeight * 0.20);
            }
            catch (Exception ex)
            {
                Debug.WriteLine($"Error positioning on screen: {ex.Message}");
            }
        }

        private void ToggleSpotlight()
        {
            if (IsVisible)
            {
                // Dacă cursorul mouse-ului este pe alt ecran decât fereastra, o mutăm direct pe acel ecran
                if (GetCursorPos(out POINT cursorPt))
                {
                    IntPtr hCursorMon = MonitorFromPoint(cursorPt, MONITOR_DEFAULTTONEAREST);
                    IntPtr handle = new WindowInteropHelper(this).Handle;
                    IntPtr hWindowMon = MonitorFromWindow(handle, MONITOR_DEFAULTTONEAREST);

                    if (hCursorMon != IntPtr.Zero && hCursorMon != hWindowMon)
                    {
                        PositionOnCurrentScreen();
                        SetForegroundWindow(handle);
                        Activate();
                        SearchBox.Focus();
                        Keyboard.Focus(SearchBox);
                        SearchBox.SelectAll();
                        UpdateCustomCaretPosition();
                        return;
                    }
                }

                HideSpotlight();
            }
            else
            {
                ShowSpotlight();
            }
        }

        private void ShowSpotlight()
        {
            PositionOnCurrentScreen();
            Show();

            IntPtr handle = new WindowInteropHelper(this).Handle;
            SetForegroundWindow(handle);
            Activate();
            Topmost = true;

            SearchBox.Focus();
            Keyboard.Focus(SearchBox);
            SearchBox.SelectAll();

            Dispatcher.BeginInvoke(System.Windows.Threading.DispatcherPriority.Input, new Action(() =>
            {
                SearchBox.Focus();
                Keyboard.Focus(SearchBox);
                UpdateCustomCaretPosition();
                StartCaretBlink();
            }));
        }

        private void SearchBox_SelectionChanged(object sender, RoutedEventArgs e)
        {
            UpdateCustomCaretPosition();
        }

        private void SearchBox_GotFocus(object sender, RoutedEventArgs e)
        {
            if (CustomCaret != null)
                CustomCaret.Visibility = Visibility.Visible;
            StartCaretBlink();
            UpdateCustomCaretPosition();
        }

        private void SearchBox_LostFocus(object sender, RoutedEventArgs e)
        {
            if (CustomCaret != null)
                CustomCaret.Visibility = Visibility.Collapsed;
        }

        private void SearchBox_PreviewMouseDown(object sender, MouseButtonEventArgs e)
        {
            Dispatcher.BeginInvoke(System.Windows.Threading.DispatcherPriority.Input, new Action(UpdateCustomCaretPosition));
        }

        private void UpdateCustomCaretPosition()
        {
            try
            {
                if (SearchBox == null || CaretTranslate == null) return;

                int index = SearchBox.CaretIndex;
                Rect rect = SearchBox.GetRectFromCharacterIndex(index, true);

                if (!rect.IsEmpty && !double.IsInfinity(rect.Left) && !double.IsNaN(rect.Left))
                {
                    CaretTranslate.X = rect.Left;
                }
                else
                {
                    CaretTranslate.X = 0;
                }

                ResetCaretBlink();
            }
            catch
            {
                if (CaretTranslate != null) CaretTranslate.X = 0;
            }
        }

        private void StartCaretBlink()
        {
            try
            {
                if (FindResource("CaretBlinkStoryboard") is System.Windows.Media.Animation.Storyboard sb)
                {
                    sb.Begin(this, isControllable: true);
                }
            }
            catch { }
        }

        private void ResetCaretBlink()
        {
            try
            {
                if (CaretElement != null)
                {
                    CaretElement.Opacity = 1.0;
                }
                if (FindResource("CaretBlinkStoryboard") is System.Windows.Media.Animation.Storyboard sb)
                {
                    sb.Seek(this, TimeSpan.Zero, System.Windows.Media.Animation.TimeSeekOrigin.BeginTime);
                    sb.Resume(this);
                }
            }
            catch { }
        }

        private void HideSpotlight()
        {
            Hide();
        }

        private void Window_Deactivated(object sender, EventArgs e)
        {
            HideSpotlight();
        }

        protected override void OnClosed(EventArgs e)
        {
            _appLifetimeCts.Cancel();

            IntPtr handle = new WindowInteropHelper(this).Handle;
            UnregisterHotKey(handle, HOTKEY_ALT_SPACE);
            UnregisterHotKey(handle, HOTKEY_CTRL_SPACE);
            UnregisterHotKey(handle, HOTKEY_ALT_ENTER);

            try
            {
                _tcpWriter = null;
                _tcpReader?.Dispose();
                _tcpStream?.Dispose();
                _tcpClient?.Dispose();
            }
            catch { }

            base.OnClosed(e);
        }
    }
}