using System;
using System.Collections.Generic;
using System.Diagnostics;
using System.Net.Http;
using System.Net.Http.Json;
using System.Runtime.InteropServices;
using System.Text.Json.Serialization;
using System.Threading;
using System.Threading.Tasks;
using System.Windows;
using System.Windows.Controls;
using System.Windows.Input;
using System.Windows.Interop;

namespace SpotlightUI
{
    public class FileResult
    {
        [JsonPropertyName("name")]
        public string Name { get; set; } = "";

        [JsonPropertyName("path")]
        public string Path { get; set; } = "";

        [JsonPropertyName("ext")]
        public string Extension { get; set; } = "";

        public string DisplayExt => string.IsNullOrEmpty(Extension) ? "FILE" : Extension.TrimStart('.').ToUpper();
    }

    public partial class MainWindow : Window
    {
        private static readonly HttpClient _http = new HttpClient { BaseAddress = new Uri("http://localhost:9876") };
        private CancellationTokenSource? _searchCts;

        // Constante și importuri Win32 pentru Global Hotkey (Alt + Space)
        private const int HOTKEY_ID = 9000;
        private const uint MOD_ALT = 0x0001;
        private const uint VK_SPACE = 0x20;
        private const int WM_HOTKEY = 0x0312;

        [DllImport("user32.dll")]
        private static extern bool RegisterHotKey(IntPtr hWnd, int id, uint fsModifiers, uint vk);

        [DllImport("user32.dll")]
        private static extern bool UnregisterHotKey(IntPtr hWnd, int id);

        public MainWindow()
        {
            InitializeComponent();

            // Poziționăm fereastra ca pe macOS (centrată orizontal, la ~20% de sus)
            Left = (SystemParameters.PrimaryScreenWidth - Width) / 2;
            Top = SystemParameters.PrimaryScreenHeight * 0.20;
        }

        protected override void OnSourceInitialized(EventArgs e)
        {
            base.OnSourceInitialized(e);
            IntPtr handle = new WindowInteropHelper(this).Handle;
            HwndSource.FromHwnd(handle)?.AddHook(HwndHook);

            // Înregistrăm Alt + Space global în Windows
            RegisterHotKey(handle, HOTKEY_ID, MOD_ALT, VK_SPACE);
        }

        private IntPtr HwndHook(IntPtr hwnd, int msg, IntPtr wParam, IntPtr lParam, ref bool handled)
        {
            if (msg == WM_HOTKEY && wParam.ToInt32() == HOTKEY_ID)
            {
                ToggleSpotlight();
                handled = true;
            }
            return IntPtr.Zero;
        }

        private void ToggleSpotlight()
        {
            if (IsVisible)
            {
                HideSpotlight();
            }
            else
            {
                Show();
                Activate();
                SearchBox.Focus();
                SearchBox.SelectAll();
            }
        }

        private void HideSpotlight()
        {
            Hide();
        }

        private void Window_Deactivated(object sender, EventArgs e)
        {
            // Se ascunde automat când dai click în afara ei (exact ca pe macOS)
            HideSpotlight();
        }

        private async void SearchBox_TextChanged(object sender, TextChangedEventArgs e)
        {
            string query = SearchBox.Text;
            PlaceholderText.Visibility = string.IsNullOrEmpty(query) ? Visibility.Visible : Visibility.Collapsed;

            _searchCts?.Cancel();
            if (string.IsNullOrWhiteSpace(query))
            {
                UpdateResultsUI(new List<FileResult>());
                return;
            }

            _searchCts = new CancellationTokenSource();
            var token = _searchCts.Token;

            try
            {
                // Debounce de 40ms pentru a nu face request la fiecare literă dacă tastezi foarte rapid
                await Task.Delay(40, token);

                var results = await _http.GetFromJsonAsync<List<FileResult>>(
                    $"/search?q={Uri.EscapeDataString(query)}", token);

                if (!token.IsCancellationRequested)
                {
                    UpdateResultsUI(results ?? new List<FileResult>());
                }
            }
            catch (TaskCanceledException) { }
            catch (Exception)
            {
                // Dacă serverul Go nu este pornit încă, pur și simplu nu afișăm eroare
            }
        }

        private void UpdateResultsUI(List<FileResult> results)
        {
            if (results.Count > 0)
            {
                ResultsList.ItemsSource = results;
                ResultsList.SelectedIndex = 0; // Selectăm automat primul rezultat
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

        // Control complet din tastatură (Săgeți Sus/Jos, Enter, Escape)
        private void Window_PreviewKeyDown(object sender, KeyEventArgs e)
        {
            if (e.Key == Key.Escape)
            {
                HideSpotlight();
                e.Handled = true;
            }
            else if (e.Key == Key.Down && ResultsList.Items.Count > 0)
            {
                if (ResultsList.SelectedIndex < ResultsList.Items.Count - 1)
                    ResultsList.SelectedIndex++;
                e.Handled = true;
            }
            else if (e.Key == Key.Up && ResultsList.Items.Count > 0)
            {
                if (ResultsList.SelectedIndex > 0)
                    ResultsList.SelectedIndex--;
                e.Handled = true;
            }
            else if (e.Key == Key.Enter && ResultsList.SelectedItem is FileResult selected)
            {
                LaunchFile(selected.Path);
                e.Handled = true;
            }
        }

        private void ResultsList_MouseDoubleClick(object sender, MouseButtonEventArgs e)
        {
            if (ResultsList.SelectedItem is FileResult selected)
            {
                LaunchFile(selected.Path);
            }
        }

        private void LaunchFile(string path)
        {
            try
            {
                HideSpotlight();
                SearchBox.Clear();
                // UseShellExecute = true îi spune Windows-ului să deschidă fișierul cu programul asociat
                Process.Start(new ProcessStartInfo(path) { UseShellExecute = true });
            }
            catch (Exception ex)
            {
                MessageBox.Show($"Nu s-a putut deschide fișierul: {ex.Message}");
            }
        }

        protected override void OnClosed(EventArgs e)
        {
            IntPtr handle = new WindowInteropHelper(this).Handle;
            UnregisterHotKey(handle, HOTKEY_ID);
            base.OnClosed(e);
        }
    }
}