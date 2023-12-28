package k8s

//"192.168.0.174:6443"

// func Test_findAddressFromRemoteCert(t *testing.T) {
// 	type args struct {
// 		address string
// 	}
// 	tests := []struct {
// 		name    string
// 		args    args
// 		want    []net.IP
// 		wantErr bool
// 	}{
// 		{
// 			name:    "test server",
// 			args:    args{address: "192.168.0.174:6443"},
// 			want:    []net.IP{net.IPv4(10, 96, 0, 1), net.IPv4(192, 168, 0, 174)},
// 			wantErr: false,
// 		},
// 	}
// 	for _, tt := range tests {
// 		t.Run(tt.name, func(t *testing.T) {
// 			got, err := findAddressFromRemoteCert(tt.args.address)
// 			if (err != nil) != tt.wantErr {
// 				t.Errorf("findAddressFromRemoteCert() error = %v, wantErr %v", err, tt.wantErr)
// 				return
// 			}
// 			if !reflect.DeepEqual(got, tt.want) {
// 				t.Errorf("findAddressFromRemoteCert() = %v, want %v", got, tt.want)
// 			}
// 		})
// 	}
// }

// //findAddressFromRemoteCert() =
// //[10.96.0.1 192.168.0.174]
// //[10.96.0.1 192.168.0.174]

// func Test_findWorkingKubernetesAddress(t *testing.T) {
// 	type args struct {
// 		configPath string
// 		inCluster  bool
// 	}
// 	tests := []struct {
// 		name    string
// 		args    args
// 		want    *kubernetes.Clientset
// 		wantErr bool
// 	}{
// 		{
// 			name: "test",
// 			args: args{
// 				configPath: "/home/dan/super-admin.conf",
// 				inCluster:  false,
// 			},
// 			wantErr: false,
// 		},
// 	}
// 	for _, tt := range tests {
// 		t.Run(tt.name, func(t *testing.T) {
// 			_, err := FindWorkingKubernetesAddress(tt.args.configPath, tt.args.inCluster)
// 			if (err != nil) != tt.wantErr {
// 				t.Errorf("findWorkingKubernetesAddress() error = %v, wantErr %v", err, tt.wantErr)
// 				return
// 			}

// 		})
// 	}
// }
